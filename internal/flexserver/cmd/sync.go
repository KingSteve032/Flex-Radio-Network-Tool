package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexserver/utils"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// ViperValidateSyncConfigOptions validates configuration options for the sync command
func ViperValidateSyncConfigOptions(c *viper.Viper) (co utils.ConfigOptions, err error) {
	flag_delete := c.GetBool("delete")
	flag_debug := c.GetBool("debug")

	co = utils.ConfigOptions{
		Mode:              "sync",
		EnableDeleteUsers: flag_delete,
		EnableDebug:       flag_debug,
	}

	// Validate NetBird API connection details
	apiPassword := c.GetString("NETBIRD_API_TOKEN")
	if apiPassword != "" {
		co.NetbirdApiConnection.Password = apiPassword
	} else {
		return co, fmt.Errorf("NETBIRD_API_TOKEN is not set in the config file or environment")
	}

	apiUrl := c.GetString("NETBIRD_API_URL")
	if apiUrl != "" {
		co.NetbirdApiConnection.Url = apiUrl
	} else {
		return co, fmt.Errorf("NETBIRD_API_URL is not set in the config file or environment")
	}

	return co, nil
}

// GetNetBirdConnectedUsers fetches all peers and returns only connected peers with valid IPs and non-blank UserID
func GetNetBirdConnectedUsers(co utils.ConfigOptions) ([]utils.VpnRouteRow, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", co.NetbirdApiConnection.Url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Token "+co.NetbirdApiConnection.Password)

	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform request: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if co.EnableDebug {
		fmt.Println("NetBird API response:", string(body))
	}

	var allPeers []utils.VpnRouteRow
	if err := json.Unmarshal(body, &allPeers); err != nil {
		return nil, fmt.Errorf("failed to unmarshal NetBird response: %w", err)
	}

	// Filter only connected peers with valid IP and non-blank UserID
	var connectedPeers []utils.VpnRouteRow
	for _, p := range allPeers {
		if p.Connected && p.IP != "" && p.Name != "UNDEF" && p.UserID != "" {
			connectedPeers = append(connectedPeers, p)
		}
	}

	return connectedPeers, nil
}

// SyncUsersFromNetBird pulls connected peers from the NetBird API and refreshes
// the local sqlite users table. Returns the number of connected peers synced.
func SyncUsersFromNetBird(co utils.ConfigOptions, deleteFirst bool) (int, error) {
	u, err := utils.UsersDb()
	if err != nil {
		return 0, fmt.Errorf("error accessing db: %w", err)
	}

	clients, err := GetNetBirdConnectedUsers(co)
	if err != nil {
		return 0, fmt.Errorf("error retrieving VPN users: %w", err)
	}

	if co.EnableDebug {
		fmt.Println("Connected peers to sync:", clients)
	}

	if deleteFirst {
		if err := u.DeleteAllUsers(); err != nil {
			return 0, fmt.Errorf("error deleting users: %w", err)
		}
	}

	var activeIPs []string
	for _, vpnUser := range clients {
		activeIPs = append(activeIPs, vpnUser.IP)
		if err := u.InsertOrKeepConnectedTime(vpnUser); err != nil {
			fmt.Printf("failed to insert VPN user %s: %v\n", vpnUser.Name, err)
		}
	}

	// Remove stale users (only if not doing full delete)
	if !deleteFirst && len(activeIPs) > 0 {
		if err := u.RemoveUsersNotInList(activeIPs); err != nil {
			fmt.Println("failed to remove stale users:", err)
		} else if co.EnableDebug {
			fmt.Println("Removed any stale users not in current NetBird list")
		}
	}

	return len(clients), nil
}

// syncCmd represents the sync command
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Syncs NetBird VPN connected users to the local client database",
	Run: func(cmd *cobra.Command, args []string) {
		viperConfig := GetConfig()
		viperConfig.BindPFlag("delete", cmd.Flags().Lookup("delete"))
		viperConfig.AutomaticEnv()

		co, err := ViperValidateSyncConfigOptions(viperConfig)
		if err != nil {
			fmt.Printf("INVALID CONFIGURATION: %s\n", err)
			return
		}

		count, err := SyncUsersFromNetBird(co, co.EnableDeleteUsers)
		if err != nil {
			fmt.Println(err)
			return
		}

		fmt.Printf("Synced %d connected peers.\n", count)
	},
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolP("delete", "d", false, "delete existing users from the database before sync")
}
