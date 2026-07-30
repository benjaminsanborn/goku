// Command platform is the local-workspace CLI: it creates projects, manages
// the platform.yaml manifest and its local cognates, and pushes changesets.
package main

import (
	"fmt"
	"os"
)

const usage = `platform — compliant CI/CD for agents

Usage:
  platform new <name>            create a project, clone it, scaffold the workspace
  platform clone <name>          clone an existing project into ./<name>
  platform add <type> <name>     add a resource to platform.yaml and start its local cognate
                                 (types: database, storage)
  platform dev                   start local cognates for everything in platform.yaml
  platform env                   print the injected environment contract
  platform run -- <cmd> [args]   run a command with the environment injected
  platform push [-t title] [-d description]
                                 push the current branch and open a changeset
  platform status                show project status and changesets

Environment:
  PLATFORM_URL    control plane URL (default http://localhost:8080)
  PLATFORM_TOKEN  auth token (default dev-token)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "new":
		err = cmdNew(os.Args[2:])
	case "clone":
		err = cmdClone(os.Args[2:])
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
