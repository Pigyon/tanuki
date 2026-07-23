// Command tanuki is a transparent anonymization proxy for AI-assisted
// security testing. It runs as the container entrypoint (tanuki proxy) and
// as the CLI for managing engagements and mappings.
package main

import "github.com/Pigyon/tanuki/internal/tanuki"

func main() {
	tanuki.Run()
}
