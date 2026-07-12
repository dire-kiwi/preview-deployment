package main

import (
	"context"
	"os"

	"github.com/dire-kiwi/preview-deployment/internal/previewcli"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	exitCode := previewcli.Run(context.Background(), os.Args[1:], previewcli.Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}, previewcli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
	os.Exit(exitCode)
}
