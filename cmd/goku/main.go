// Command goku is the local-workspace CLI: it creates projects, manages
// the goku.yaml manifest and its local cognates, and pushes changesets.
package main

import (
	"fmt"
	"os"
)

// version is stamped by goreleaser via ldflags.
var version = "dev"

const usage = `goku — compliant CI/CD for agents

Usage:
  goku login                 authenticate with your organization token
  goku whoami                show which org you're authenticated as
  goku new <name>            create a project, clone it, scaffold the workspace
  goku import <repo> [name]  import a GitHub repo as a project (history preserved)
  goku clone <name>          clone an existing project into ./<name>
  goku sync [name]           pull latest from a linked GitHub repo into goku
  goku add <type> <name>     add a resource to goku.yaml and start its local cognate
                                 (types: database, storage)
  goku dev                   start local cognates for everything in goku.yaml
  goku env                   print the injected environment contract
  goku run -- <cmd> [args]   run a command with the environment injected
  goku push                  push the current branch for review
  goku status                show project status and branches
  goku mcp                   serve MCP over stdio for Claude (registered by login)

Environment:
  GOKU_URL    control plane URL (default http://localhost:8080)
  GOKU_TOKEN  auth token (default dev-token)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(os.Args[2:])
	case "whoami":
		err = cmdWhoami()
	case "mcp":
		err = cmdMCP()
	case "version", "-v", "--version":
		fmt.Println("goku", version)
	case "new":
		err = cmdNew(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "clone":
		err = cmdClone(os.Args[2:])
	case "sync":
		err = cmdSync(os.Args[2:])
	case "add":
		err = cmdAdd(os.Args[2:])
	case "dev":
		err = cmdDev()
	case "env":
		err = cmdEnv()
	case "run":
		err = cmdRun(os.Args[2:])
	case "push":
		err = cmdPush(os.Args[2:])
	case "status":
		err = cmdStatus()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Printf("unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
