/*
Copyright © 2023 Blair Gillam <ns1h@airmada.net>
Reconfigured for Netbird by Steven Griggs <kc4caw@w4car.org>
*/
package cmd

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexserver/utils"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/littleairmada/vrt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// ViperValidatePcapConfigOptions validates the configuration options used during 'pcap' mode
func ViperValidatePcapConfigOptions(mode string, c *viper.Viper) (co utils.ConfigOptions, err error) {
	flag_broadcast := c.GetBool("broadcast")
	flag_broadcastport := c.GetInt("port")
	flag_debug := c.GetBool("debug")
	flag_bpffilter := c.GetString("filter")
	flag_interface := c.GetString("interface")
	flag_pcapfile := c.GetString("pcapfile")

	co = utils.ConfigOptions{}
	// validate MODE
	switch mode {
	case "info":
		co.Mode = "info"
	case "pcap":
		co.Mode = "pcap"
	case "listen":
		co.Mode = "listen"
	default:
		err := fmt.Errorf("the requested mode \"%s\" is not a valid mode", mode)
		return utils.ConfigOptions{}, err
	}

	// validate pcapFile if co.Mode is pcap
	if co.Mode == "pcap" {
		if _, err := os.Stat(flag_pcapfile); err == nil {
			co.PcapFile = flag_pcapfile

		} else if errors.Is(err, os.ErrNotExist) {
			err := fmt.Errorf("the requested pcapfile \"%s\" does not exist", flag_pcapfile)
			return co, err

		} else {
			return co, err
		}
	}

	// validate EnableBroadcast
	switch flag_broadcast {
	case true:
		co.EnableBroadcast = true
		tempNetworkInterface, err := utils.ValidateNetworkInterfaceByName(flag_interface)
		if err != nil {
			return co, err
		}
		co.NetworkInteface = tempNetworkInterface
	default:
		co.EnableBroadcast = false
	}

	// validate flag_broadcastport
	if math.Signbit(float64(flag_broadcastport)) || flag_broadcastport >= 65536 {
		fmt.Println("Port number must be a valid port between 0 and 65535")
		return utils.ConfigOptions{}, err
	} else {
		co.BroadcastPort = flag_broadcastport
	}

	// validate EnableDebug
	switch flag_debug {
	case true:
		co.EnableDebug = true
	default:
		co.EnableDebug = false
	}

	if flag_bpffilter != "" {
		co.BPFFilter = flag_bpffilter
	} else {
		co.BPFFilter = "udp and port 4992 and dst host 255.255.255.255"
	}

	return co, nil
}

// ParsePcapFile parses the given pcap file for VRT packets. Error is returned as needed.
func ParsePcapFile(co utils.ConfigOptions) error {
	fmt.Println("Parsing PCAP file...")
	if _, err := os.Stat(co.PcapFile); err == nil {
		handle, err := pcap.OpenOffline(co.PcapFile)
		if err != nil {
			log.Fatal(err)
		}
		defer handle.Close()

		err = handle.SetBPFFilter(co.BPFFilter)
		if err != nil {
			log.Fatal(err)
		}

		// Loop through packets in file
		packetSource := gopacket.NewPacketSource(handle, handle.LinkType())

		for packet := range packetSource.Packets() {
			udpLayer := packet.Layer(layers.LayerTypeUDP)
			if udpLayer != nil {
				vrtStruct := vrt.VRT{}
				udp, _ := udpLayer.(*layers.UDP)
				if udp.Payload != nil && len(udpLayer.LayerPayload()) > 0 {
					err := vrtStruct.DecodeFromBytes(udp.Payload, gopacket.NilDecodeFeedback)
					if err != nil {
						fmt.Println("Error decoding UDP packet as VRT: ", err)
					} else {
						if co.EnableDebug {
							utils.PrintVrtPacket(vrtStruct)
						}
						if !co.EnableBroadcast {
							fmt.Println("Send Discovery Packet Disabled")
						} else {
							utils.MaybeSendDiscoveryPacket(co, vrtStruct)
						}
					}
				}
			}
		}

	} else if errors.Is(err, os.ErrNotExist) {
		fmt.Printf("PCAP file %s does not exist or cannot be read", co.PcapFile)
		os.Exit(0)
	} else {
		fmt.Println("So a wierd thing just happened. We may have a Schrodinger file: the file may or may not exist.")
		fmt.Println("All I know is the error is:")
		fmt.Println(err)
	}

	return nil
}

// pcapCmd represents the pcap command
var pcapCmd = &cobra.Command{
	Use:   "pcap",
	Short: "Processes FlexRadio Discovery Packets from a packet capture file",
	Long: `
This command causes flextool to read a packet capture file for FlexRadio Discovery
Packets on 4992/udp and retransmit them as UDP unicast packets to a list 
of client IP addresses.

Read test.pcap and broadcast FlexRadio Discovery packets using eth0:
flextool pcap -f test.pcap -i eth0 -b

Print out FlexRadio Discovery packets from test.pcap:
flextool pcap -f test.pcap`,
	PreRun: func(cmd *cobra.Command, args []string) {
		broadcast_flag, _ := cmd.Flags().GetBool("broadcast")
		if broadcast_flag {
			cmd.MarkFlagRequired("interface")
		}
	},
	Run: func(cmd *cobra.Command, args []string) {
		viperConfig := GetConfig()
		viperConfig.BindPFlag("pcapfile", cmd.Flags().Lookup("pcapfile"))
		viperConfig.BindPFlag("broadcast", cmd.Flags().Lookup("broadcast"))
		viperConfig.BindPFlag("debug", cmd.Flags().Lookup("debug"))
		viperConfig.BindPFlag("interface", cmd.Flags().Lookup("interface"))
		viperConfig.BindPFlag("filter", cmd.Flags().Lookup("filter"))
		viperConfig.BindPFlag("port", cmd.Flags().Lookup("port"))

		viperConfig.AutomaticEnv()

		// Validate configuration options
		co, err := ViperValidatePcapConfigOptions("pcap", viperConfig)
		if err != nil {
			fmt.Printf("INVALID CONFIGURTAION ERROR: %s\n", err)
			return
		}

		// Parse PCAP file
		ParsePcapFile(co)
	},
}

func init() {
	rootCmd.AddCommand(pcapCmd)

	// Local Flags
	pcapCmd.Flags().StringP("pcapfile", "f", "", "Packet capture file (*.pcap or *.pcapng)")
	pcapCmd.Flags().BoolP("broadcast", "b", false, "Broadcast discovery packets")
	pcapCmd.Flags().BoolP("debug", "d", false, "Print debug messages")
	pcapCmd.Flags().StringP("interface", "i", "", "Network interface to rebroadcast packets on")
	pcapCmd.Flags().String("filter", "udp and port 4992 and dst host 255.255.255.255", "Berkley packet filter rule to match packets against. Defaults to: udp and port 4992 and dst host 255.255.255.255")
	pcapCmd.Flags().IntVarP(&broadcastPort, "port", "p", 14992, "UDP port to broadcast FlexRadio discovery packets to")
}
