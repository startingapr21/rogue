package main

import (
	"github.com/startingapr21/rogue/pkg/cli"
	"github.com/startingapr21/rogue/pkg/util/console"
)

func main() {
	cmd, err := cli.NewRootCommand()
	if err != nil {
		console.Fatalf("%f", err)
	}

	if err = cmd.Execute(); err != nil {
		console.Fatalf("%s", err)
	}
}
