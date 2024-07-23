package docker

import (
	"os"
	"os/exec"
	"strings"

	"github.com/startingapr21/rogue/pkg/util/console"
)

func Push(image string) error {
	cmd := exec.Command(
		"docker", "push", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	console.Debug("$ " + strings.Join(cmd.Args, " "))
	return cmd.Run()
}
