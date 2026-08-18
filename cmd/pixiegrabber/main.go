// Command pixiegrabber preserves authorized Pixieset Client Gallery
// Collections locally.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"pixiegrabber/internal/app"
)

func main() {
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "pixiegrabber: Windows is not supported; use macOS or Linux")
		os.Exit(1)
	}
	var options app.Options
	flag.StringVar(&options.CookiesFromBrowser, "cookies-from-browser", "", "import the active Pixieset session from BROWSER[:PROFILE][::CONTAINER]")
	flag.StringVar(&options.Output, "output", "", "select the local output root (required unless --s3)")
	flag.BoolVar(&options.SyncExisting, "sync-existing", false, "refresh completed Collections and represent remote removals without deleting local files")
	flag.BoolVar(&options.Verify, "verify", false, "check every local Placement against its saved SHA-256 checksum and restore missing or changed files")
	flag.BoolVar(&options.Yes, "yes", false, "accept the download plan without an interactive prompt")
	flag.IntVar(&options.Concurrency, "concurrency", 4, "set concurrent Reference downloads")
	flag.StringVar(&options.UserAgent, "user-agent", "", "override the User-Agent detected from the selected browser")
	flag.BoolVar(&options.S3, "s3", false, "store output in an S3-compatible bucket instead of a local directory")
	flag.StringVar(&options.S3Endpoint, "s3-endpoint", "", "S3-compatible endpoint as host[:port] without a scheme")
	flag.StringVar(&options.S3Bucket, "s3-bucket", "", "S3 bucket name (must already exist)")
	flag.StringVar(&options.S3Region, "s3-region", "us-east-1", "S3 region")
	flag.BoolVar(&options.S3PathStyle, "s3-path-style", true, "use path-style S3 addressing")
	flag.BoolVar(&options.S3Secure, "s3-secure", true, "use HTTPS for the S3 endpoint")
	var interval string
	flag.StringVar(&interval, "interval", "0", "minimum delay between API and media requests (e.g. 2s); 0 disables throttling")
	flag.Parse()

	if interval != "" {
		parsed, err := time.ParseDuration(interval)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pixiegrabber: invalid --interval: %v\n", err)
			os.Exit(1)
		}
		options.Interval = parsed
	}

	if !options.S3 && options.Output == "" {
		fmt.Fprintln(os.Stderr, "pixiegrabber: --output is required unless --s3 is set")
		os.Exit(1)
	}

	if err := app.Run(context.Background(), options, os.Stdout, os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "pixiegrabber: %v\n", err)
		os.Exit(1)
	}
}
