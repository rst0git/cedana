package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/checkpoint-restore/go-criu"
	pb "github.com/nravic/cedana/rpc"
	"github.com/nravic/cedana/utils"
	"github.com/rs/zerolog"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Client struct {
	CRIU          *criu.Criu
	rpcClient     pb.CedanaClient
	rpcConnection *grpc.ClientConn
	logger        *zerolog.Logger
	config        *utils.Config
	channels      *CommandChannels
	context       context.Context
}

type CommandChannels struct {
	dump_command    chan int
	restore_command chan int
}

var clientCommand = &cobra.Command{
	Use:   "client",
	Short: "Directly dump/restore a process or start a daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("error: must also specify dump, restore or daemon")
	},
}

func UnaryClientInterceptor(
	ctx context.Context,
	method string,
	req interface{},
	reply interface{},
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	jwt, exists := os.LookupEnv("CEDANA_JWT_TOKEN")
	if !exists {
		return fmt.Errorf("JWT env var unset! Something likely went wrong during instance setup")
	}

	authCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "bearer"+jwt)
	err := invoker(authCtx, method, req, reply, cc, opts...)

	// TODO: Need way to refresh JWT token
	if status.Code(err) == codes.Unauthenticated {
		return fmt.Errorf("JWT token expired")
	}

	return err
}

func instantiateClient() (*Client, error) {
	// instantiate logger
	logger := utils.GetLogger()

	c := criu.MakeCriu()
	_, err := c.GetCriuVersion()
	if err != nil {
		logger.Fatal().Err(err).Msg("Error checking CRIU version")
		return nil, err
	}
	// prepare client
	err = c.Prepare()
	if err != nil {
		logger.Fatal().Err(err).Msg("Error preparing CRIU client")
		return nil, err
	}

	config, err := utils.InitConfig()
	if err != nil {
		logger.Fatal().Err(err).Msg("Could not read config")
		return nil, err
	}

	var opts []grpc.DialOption

	// Transport credentials are intentionally insecure here - JWT takes over
	opts = append(
		opts,
		grpc.WithUnaryInterceptor(UnaryClientInterceptor),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.Dial(fmt.Sprintf("%v:%d", config.Connection.ServerAddr, config.Connection.ServerPort), opts...)
	if err != nil {
		logger.Fatal().Err(err).Msg("Could not connect to RPC server")
	}
	rpcClient := pb.NewCedanaClient(conn)

	// set up channels for daemon to listen on
	dump_command := make(chan int)
	restore_command := make(chan int)
	channels := &CommandChannels{dump_command, restore_command}
	return &Client{c, rpcClient, conn, &logger, config, channels, context.Background()}, nil
}

func (c *Client) cleanupClient() error {
	c.CRIU.Cleanup()
	c.rpcConnection.Close()
	c.logger.Info().Msg("cleaning up client")
	return nil
}

func (c *Client) registerRPCClient(pid int) {
	ctx, cancel := context.WithTimeout(c.context, 10*time.Second)
	defer cancel()

	state := c.getState(pid)
	resp, err := c.rpcClient.RegisterClient(ctx, state)
	if err != nil {
		c.logger.Fatal().Msgf("client.RegisterClient failed: %v", err)
	}
	c.logger.Info().Msgf("Response from orchestrator: %v", resp)
}

func (c *Client) recordState() {
	state := c.getState(pid)
	stream, err := c.rpcClient.RecordState(c.context)
	if err != nil {
		c.logger.Fatal().Err(err).Msgf("Could not open stream to send state")
	}

	quit := make(chan struct{})
	ticker := time.NewTicker(5 * time.Second)

	go func() {
		for {
			select {
			case <-ticker.C:
				c.logger.Debug().Msgf("sending state: %v", state)
				err := stream.Send(state)
				if err != nil {
					c.logger.Fatal().Err(err).Msg("Error sending state to orchestrator")
				}
			case <-quit:
				ticker.Stop()
				return
			default:
			}
		}
	}()
}

func (c *Client) pollForCommand(pid int) {
	// TODO: should fail gracefully
	refresher := time.NewTicker(10 * time.Second)
	defer refresher.Stop()

	waitc := make(chan struct{})

	for {
		select {
		case <-refresher.C:
			c.logger.Debug().Msgf("polling for command at %v", time.Now().Local())
			stream, err := c.rpcClient.PollForCommand(c.context)
			if err != nil {
				c.logger.Info().Msgf("could not reach orchestrator server: %v", err)
			} else {
				state := c.getState(pid)
				c.logger.Debug().Msgf("Sent state %v:", state)
				err = stream.Send(state)
				if err != nil {
					c.logger.Info().Msgf("could not reach orchestrator server: %v", err)
				}

				resp, err := stream.Recv()
				c.logger.Info().Msgf("received response: %s", resp.String())
				if err == io.EOF {
					// read done
					close(waitc)
					return
				}

				if resp == nil {
					// do nothing if we don't have a command yet
					return
				}

				if err != nil {
					c.logger.Fatal().Err(err).Msg("client.pollForCommand failed")
				}
				if resp.Checkpoint {
					c.logger.Info().Msgf("checkpoint command received: %v", resp)
					c.channels.dump_command <- 1
				}
				if resp.Restore {
					c.logger.Info().Msgf("restore command received: %v", resp)
					c.channels.restore_command <- 1
				}
			}
		default:
			// do nothing otherwise (don't block for loop)
		}
	}
}

func (c *Client) timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	c.logger.Debug().Msgf("%s took %s", name, elapsed)
}

func (c *Client) getState(pid int) *pb.ClientData {

	m, _ := mem.VirtualMemory()
	h, _ := host.Info()

	id := os.Getenv("CEDANA_CLIENT_ID")

	// ignore sending network for now, little complicated
	data := &pb.ClientData{
		ProcessInfo: &pb.ProcessInfo{
			ProcessPid: uint32(pid),
		},
		ClientInfo: &pb.ClientInfo{
			Id:       id,
			Os:       h.OS,
			Platform: h.Platform,
		},
		State: &pb.ClientState{
			RemainingMemory: int32(m.Free),
			Uptime:          int32(h.Uptime),
		},
	}

	return data
}

func init() {
	rootCmd.AddCommand(clientCommand)
}
