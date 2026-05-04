/*
Copyright © 2023 Blair Gillam <ns1h@airmada.net>
Reconfigured for Netbird by Steven Griggs <kc4caw@w4car.org>
*/
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Displays the flextool version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("0.0.3")
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
