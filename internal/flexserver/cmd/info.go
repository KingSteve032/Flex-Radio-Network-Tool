/*
Copyright © 2023 Blair Gillam <ns1h@airmada.net>
Reconfigured for Netbird by Steven Griggs <kc4caw@w4car.org>
*/
package cmd

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexserver/utils"
	"github.com/spf13/cobra"
)

// listAllNetworkInterfaces prints a list of all network interfaces on the system
func listAllNetworkInterfaces() {
	interfaces, err := net.Interfaces()
	if err != nil {
		fmt.Println("Error accessing network intefaces: ", err.Error())
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.TabIndent)
	fmt.Fprintln(w, "Interface Id\tInterface Name\tHardware Address\tIP Addresses")
	fmt.Fprintln(w, "============\t==============\t================\t============")
	for _, interf := range interfaces {
		var addrs_output string
		if addrs, err := interf.Addrs(); err == nil {
			for _, addr := range addrs {
				addrs_output = addrs_output + " " + addr.String()
			}
		}
		fmt.Fprintln(w, strconv.FormatInt(int64(interf.Index), 10)+"\t"+interf.Name+"\t"+interf.HardwareAddr.String()+"\t"+addrs_output)
	}
	w.Flush()
}

// infoCmd represents the info command
var infoCmd = &cobra.Command{
	Use:   "info",
	Short: "Displays information about the network interfaces",
	Long: `Displays information about the system's network interfaces.

List all network interfaces:
./flextool info list

Get the details of a specific network interface:
./flextool info get -i eth0`,
	PreRun: func(cmd *cobra.Command, args []string) {
		get_flag, _ := cmd.Flags().GetBool("get")
		if get_flag {
			cmd.MarkFlagRequired("interface")
		}
	},
	Run: func(cmd *cobra.Command, args []string) {
		get_flag, _ := cmd.Flags().GetBool("get")
		list_flag, _ := cmd.Flags().GetBool("list")
		if list_flag {
			listAllNetworkInterfaces()
		} else if get_flag {
			ifname, _ := cmd.Flags().GetString("interface")
			fmt.Println(ifname)
			utils.GetNetworkInterfaceByName(ifname)
		} else {
			fmt.Println("Invalid command arguments")
			cmd.Help()
		}
	},
}

func init() {
	rootCmd.AddCommand(infoCmd)

	infoCmd.Flags().BoolP("list", "l", false, "lists all available network interfaces")
	infoCmd.Flags().BoolP("get", "g", false, "get details for single network interfaces")
	infoCmd.Flags().StringP("interface", "i", "", "Network interface name")
}
