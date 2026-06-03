package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	internalcmd "github.com/router-for-me/CLIProxyAPI/v7/internal/cmd"
)

type pathList []string

func (p *pathList) String() string {
	if p == nil {
		return ""
	}
	return strings.Join(*p, ",")
}

func (p *pathList) Set(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*p = append(*p, part)
		}
	}
	return nil
}

func main() {
	var paths pathList
	var provider string
	var format string
	var recursive bool
	var noHeader bool

	flag.Var(&paths, "path", "auth JSON file or directory to scan; may be repeated or comma-separated")
	flag.StringVar(&provider, "provider", "codex", "provider filter; empty disables filtering")
	flag.StringVar(&format, "format", "tsv", "output format: tsv or jsonl")
	flag.BoolVar(&recursive, "recursive", true, "recursively scan directories")
	flag.BoolVar(&noHeader, "no-header", false, "omit TSV header")
	flag.Parse()

	err := internalcmd.RunAuthTokenFingerprint(context.Background(), os.Stdout, internalcmd.AuthTokenFingerprintOptions{
		Paths:     paths,
		Provider:  provider,
		Recursive: recursive,
		Format:    format,
		NoHeader:  noHeader,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "auth-token-fingerprint: %v\n", err)
		os.Exit(1)
	}
}
