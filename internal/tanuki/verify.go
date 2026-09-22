package tanuki

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// verifyTemplates are the shapes a target takes in practice. A mapping that
// covers the bare value can still fail inside one of these -- the automaton
// used to match a hostname inside a longer name, for instance -- so each real
// value is checked in all of them rather than on its own.
var verifyTemplates = []struct{ name, format string }{
	{"bare", "%s"},
	{"in a sentence", "Scanning %s for open ports."},
	{"end of sentence", "Reached %s. Moving on."},
	{"url", "curl -sS https://%s/api/v1/users"},
	{"markdown link", "[target](https://%s/login)"},
	{"backticks", "the host `%s` responded with 403"},
	{"json", `{"target":"%s","status":"up"}`},
	{"http header", "Host: %s\r\nUser-Agent: curl/8.0"},
	{"log line", "2026-09-22T10:00:00Z INFO connected to %s (retry 0)"},
	{"upper case", "%s"},
	{"comma separated", "hosts: %s, example.invalid"},
	{"parenthesised", "the target (%s) is in scope"},
}

// cmdVerify checks that an engagement's mappings actually hold. With no
// arguments it exercises every real value in the table through the shapes
// above. Given files or piped input it reports the real-looking values in that
// text which have no mapping, which is the check worth running over a draft
// report before it is submitted.
func cmdVerify(args []string) {
	eng, err := requireEngagement()
	if err != nil {
		fatal(err.Error())
	}

	engDir := engagementDir(eng)

	mappings := loadMappings(engDir)
	if len(mappings) == 0 {
		fatal("no mappings to verify (run: tanuki add <domain>)")
	}

	if len(args) > 0 || redirectedStdin() {
		os.Exit(verifyInput(args, newRewriter(mappings, directionR2F)))
	}

	os.Exit(verifyMappings(mappings))
}

// redirectedStdin reports whether stdin is a regular file, which is what
// "tanuki verify < report.md" gives. A pipe is deliberately not detected:
// reading one that nobody writes to blocks forever, and "docker run -i" hands
// the command exactly that. Pipe input is spelled "tanuki verify -", where the
// wait is what the operator asked for.
func redirectedStdin() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}

	return info.Mode().IsRegular()
}

// verifyMappings renders every real value in each shape, rewrites it, and
// reports the ones that survive. It returns the process exit code.
func verifyMappings(mappings []mappingEntry) int {
	rw := newRewriter(mappings, directionR2F)
	leaks := 0
	checks := 0

	fmt.Printf("Verifying %d mappings against %d shapes\n\n", len(mappings), len(verifyTemplates))

	for _, m := range mappings {
		realVal := strings.TrimPrefix(m.Real, "*")
		if realVal == "" {
			continue
		}

		for _, tmpl := range verifyTemplates {
			value := realVal
			if tmpl.name == "upper case" {
				value = strings.ToUpper(realVal)
			}

			checks++

			text := fmt.Sprintf(tmpl.format, value)

			got := rw.rewrite(text)
			if !strings.Contains(strings.ToLower(got), strings.ToLower(realVal)) {
				continue
			}

			leaks++

			fmt.Printf("  LEAK  %-8s %-28s %s\n", m.Type, realVal, tmpl.name)
			fmt.Printf("        in:  %s\n", singleLine(text))
			fmt.Printf("        out: %s\n", singleLine(got))
		}
	}

	fmt.Printf("\n%d checks, %d leaks\n", checks, leaks)

	if leaks > 0 {
		fmt.Println("A real value survived rewriting. Do not rely on this engagement until it is fixed.")

		return exitError
	}

	fmt.Println("Every mapped value was rewritten in every shape.")

	return 0
}

// verifyInput reports the real-looking values in the given files (or stdin)
// that no mapping covers. Rewriting first means a value with a mapping is
// already gone, so whatever the scan still finds is genuinely unprotected.
func verifyInput(paths []string, rw *rewriter) int {
	unmapped := 0

	for _, source := range readSources(paths) {
		residual := residualTargets(rw.rewrite(source.text))
		if len(residual) == 0 {
			fmt.Printf("  ok    %s\n", source.name)
			continue
		}

		unmapped += len(residual)

		fmt.Printf("  LEAK  %s\n", source.name)

		for _, v := range residual {
			fmt.Printf("        %s\n", v)
		}
	}

	if unmapped > 0 {
		fmt.Printf("\n%d value(s) have no mapping and would be sent as-is.\n", unmapped)
		fmt.Println("Add them with: tanuki add <domain> / tanuki add-ip <ip>")

		return exitError
	}

	fmt.Println("\nNo unmapped targets found.")

	return 0
}

type verifySource struct{ name, text string }

func readSources(paths []string) []verifySource {
	if len(paths) == 0 {
		paths = []string{"-"}
	}

	sources := make([]verifySource, 0, len(paths))

	for _, path := range paths {
		if path == "-" {
			data, err := io.ReadAll(bufio.NewReader(os.Stdin))
			if err != nil {
				fatal(fmt.Sprintf("reading stdin: %v", err))
			}

			sources = append(sources, verifySource{name: "(stdin)", text: string(data)})

			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			fatal(fmt.Sprintf("reading %s: %v", path, err))
		}

		sources = append(sources, verifySource{name: path, text: string(data)})
	}

	return sources
}

// singleLine keeps a sample on one output line so the report stays readable.
func singleLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", "\\r"), "\n", "\\n")
}
