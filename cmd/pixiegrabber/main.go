// Command pixiegrabber preserves authorized Pixieset Client Gallery
// Collections locally.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"

	"pixiegrabber/internal/app"
)

func main() {
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "pixiegrabber: Windows is not supported; use macOS or Linux")
		os.Exit(1)
	}
	var options app.Options
	flag.StringVar(&options.CookiesFromBrowser, "cookies-from-browser", "", "import the active Pixieset session from BROWSER[:PROFILE][::CONTAINER]")
	flag.StringVar(&options.Output, "output", "", "select the local output root (required)")
	flag.BoolVar(&options.SyncExisting, "sync-existing", false, "refresh completed Collections and represent remote removals without deleting local files")
	flag.BoolVar(&options.Verify, "verify", false, "check every local Placement against its saved SHA-256 checksum and restore missing or changed files")
	flag.BoolVar(&options.Yes, "yes", false, "accept the download plan without an interactive prompt")
	flag.IntVar(&options.Concurrency, "concurrency", 4, "set concurrent Reference downloads")
	flag.StringVar(&options.UserAgent, "user-agent", "", "override the User-Agent detected from the selected browser")
	flag.Parse()

	if err := app.Run(context.Background(), options, os.Stdout, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "pixiegrabber: %v\n", err)
		os.Exit(1)
	}
}
