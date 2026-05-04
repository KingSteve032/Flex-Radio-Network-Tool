package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	flexservercmd "github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexserver/cmd"
	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/frnt"
)

func main() {
	mode, remainingArgs, err := parseModeArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "argument error:", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	switch mode {
	case "client":
		if len(remainingArgs) > 0 {
			fmt.Fprintf(os.Stderr, "client mode does not accept extra arguments: %s\n", strings.Join(remainingArgs, " "))
			printUsage(os.Stderr)
			os.Exit(2)
		}
		frnt.Run()
	case "server":
		if len(remainingArgs) == 0 {
			remainingArgs = []string{"--help"}
		}
		if err := flexservercmd.ExecuteWithArgs(remainingArgs); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", mode)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func parseModeArgs(args []string) (mode string, remaining []string, err error) {
	mode = defaultMode()
	remaining = make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--mode" || arg == "-mode":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a value", arg)
			}
			mode = strings.ToLower(strings.TrimSpace(args[i+1]))
			i++
		case strings.HasPrefix(arg, "--mode="):
			mode = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "--mode=")))
		case strings.HasPrefix(arg, "-mode="):
			mode = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(arg, "-mode=")))
		default:
			remaining = append(remaining, arg)
		}
	}

	if mode == "" {
		return "", nil, fmt.Errorf("mode cannot be empty")
	}
	if mode != "client" && mode != "server" {
		return "", nil, fmt.Errorf("invalid mode %q (supported: client, server)", mode)
	}

	return mode, remaining, nil
}

func defaultMode() string {
	if runtime.GOOS == "linux" {
		return "server"
	}
	return "client"
}

func printUsage(out *os.File) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  Flex-Radio-Network-Tool --mode client")
	fmt.Fprintln(out, "  Flex-Radio-Network-Tool --mode server <subcommand> [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Server subcommands: sync, listen, info, pcap, version")
}
