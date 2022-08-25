package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/checkpoint-restore/go-criu"
	"github.com/checkpoint-restore/go-criu/rpc"
	"github.com/nravic/oort/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/protobuf/proto"
)

var clientRestoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Initialize client and restore from dumped image",
	RunE: func(cmd *cobra.Command, args []string) error {
		// want to be able to get the criu object from the root, but that's neither here nor there
		c, err := instantiateClient()
		if err != nil {
			return err
		}
		err = c.restore()
		if err != nil {
			return err
		}
		return nil
	},
}

func (c *Client) restore() error {
	utils.InitConfig()
	img, err := os.Open(viper.GetString("dump_storage_location"))
	if err != nil {
		return fmt.Errorf("can't open image dir")
	}
	defer img.Close()

	opts := rpc.CriuOpts{
		ImagesDirFd: proto.Int32(int32(img.Fd())),
		LogLevel:    proto.Int32(4),
		LogFile:     proto.String("restore.log"),
		ShellJob:    proto.Bool(true),
	}

	// TODO: restore needs to do some work here (restoring connections?)
	err = c.CRIU.Restore(opts, criu.NoNotify{})
	if err != nil {
		log.Fatal("Error restoring process!", err)
		return err
	}

	c.cleanupClient()

	return nil
}

func init() {
	clientCommand.AddCommand(clientRestoreCmd)
}
