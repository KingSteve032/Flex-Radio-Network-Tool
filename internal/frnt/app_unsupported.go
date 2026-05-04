//go:build !windows && !darwin

package frnt

import "fmt"

// Run is only available on desktop platforms with GUI support.
func Run() {
	fmt.Println("client mode is currently supported on Windows and macOS only")
}
