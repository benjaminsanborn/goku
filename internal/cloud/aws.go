// Package cloud talks to the provider APIs behind goku's cloud accounts:
// credential verification for every kind, and — for AWS — provisioning the
// EC2 instances that deployments actually land on.
//
// It speaks the wire protocols directly (SigV4 over the EC2 query API and the
// SSM JSON API) rather than pulling in the AWS SDK, which keeps the control
// plane's dependency surface to the standard library.
package cloud

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AWS is a set of credentials for one region.
type AWS struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Region          string
}

// AWSFrom builds a client from stored credentials, defaulting the region.
func AWSFrom(creds map[string]string, region string) AWS {
	if region == "" {
		region = "us-east-1"
	}
	return AWS{
		AccessKeyID:     creds["access_key_id"],
		SecretAccessKey: creds["secret_access_key"],
		SessionToken:    creds["session_token"],
		Region:          region,
	}
}

// Identity proves the credentials are live, returning the caller's ARN.
func (a AWS) Identity(ctx context.Context) (string, error) {
	var out struct {
		Arn string `xml:"GetCallerIdentityResult>Arn"`
	}
	if err := a.query(ctx, "sts", url.Values{"Action": {"GetCallerIdentity"}, "Version": {"2011-06-15"}}, &out); err != nil {
		return "", err
	}
	if out.Arn == "" {
		return "aws account", nil
	}
	return out.Arn, nil
}

// query calls one of the form-encoded XML APIs (sts, ec2).
func (a AWS) query(ctx context.Context, service string, form url.Values, out any) error {
	host := service + "." + a.Region + ".amazonaws.com"
	body := form.Encode()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+host+"/", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.sign(req, body, service)

	raw, err := do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", service, awsError(raw, err))
	}
	if out == nil {
		return nil
	}
	if err := xml.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: parse response: %w", service, err)
	}
	return nil
}

// jsonAPI calls one of the JSON-protocol APIs (ssm), which dispatch on a
// target header rather than an Action parameter.
func (a AWS) jsonAPI(ctx context.Context, service, target string, in, out any) error {
	host := service + "." + a.Region + ".amazonaws.com"
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+host+"/", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", target)
	a.sign(req, string(body), service)

	raw, err := do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", service, awsError(raw, err))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func do(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s", resp.Status)
	}
	return raw, nil
}

// awsError pulls the human-readable message out of an error body, falling
// back to the HTTP status when the shape is unfamiliar.
func awsError(raw []byte, fallback error) error {
	var xmlErr struct {
		Message string `xml:"Errors>Error>Message"`
		Alt     string `xml:"Error>Message"`
	}
	if xml.Unmarshal(raw, &xmlErr) == nil {
		if msg := cmp(xmlErr.Message, xmlErr.Alt); msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	var jsonErr struct {
		Message string `json:"message"`
		Alt     string `json:"Message"`
	}
	if json.Unmarshal(raw, &jsonErr) == nil {
		if msg := cmp(jsonErr.Message, jsonErr.Alt); msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	return fallback
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sign adds SigV4 authorization headers. Only host, x-amz-date and (when
// present) the security token and target are signed — AWS permits other
// headers to travel unsigned.
func (a AWS) sign(req *http.Request, body, service string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	headers := []struct{ name, value string }{
		{"host", req.URL.Host},
		{"x-amz-date", amzDate},
	}
	if a.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", a.SessionToken)
		headers = append(headers, struct{ name, value string }{"x-amz-security-token", a.SessionToken})
	}
	if target := req.Header.Get("X-Amz-Target"); target != "" {
		headers = append(headers, struct{ name, value string }{"x-amz-target", target})
	}
	// Canonical headers must be sorted by name.
	sortHeaders(headers)
	var canonicalHeaders, signedList strings.Builder
	for i, h := range headers {
		fmt.Fprintf(&canonicalHeaders, "%s:%s\n", h.name, h.value)
		if i > 0 {
			signedList.WriteByte(';')
		}
		signedList.WriteString(h.name)
	}
	signedHeaders := signedList.String()

	canonical := strings.Join([]string{
		"POST", "/", "", canonicalHeaders.String(), signedHeaders, sha256hex(body),
	}, "\n")
	scope := dateStamp + "/" + a.Region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, sha256hex(canonical)}, "\n")

	key := hmacSHA256([]byte("AWS4"+a.SecretAccessKey), dateStamp)
	key = hmacSHA256(key, a.Region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		a.AccessKeyID, scope, signedHeaders, signature))
}

func sortHeaders(h []struct{ name, value string }) {
	for i := 1; i < len(h); i++ {
		for j := i; j > 0 && h[j].name < h[j-1].name; j-- {
			h[j], h[j-1] = h[j-1], h[j]
		}
	}
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
