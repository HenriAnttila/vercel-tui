package main

import (
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: vtui [dir]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Shows deployments for the Vercel project linked in dir (default: current directory).")
		fmt.Fprintln(os.Stderr, "Reads .vercel/project.json from dir or a parent, and the token from VERCEL_TOKEN or the Vercel CLI login.")
	}
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	link, _, err := findProjectLink(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vtui:", err)
		os.Exit(1)
	}
	token, err := loadToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vtui:", err)
		os.Exit(1)
	}

	if _, err := tea.NewProgram(newModel(newClient(token, link.teamID()), link)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "vtui:", err)
		os.Exit(1)
	}
}
