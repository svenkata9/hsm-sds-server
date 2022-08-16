package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"github.com/spf13/cobra"
	"istio.io/pkg/log"

	"github.com/intel-innersource/applications.services.cloud.hsm-sds-server/security"
)

const (
	maxStreams       = 100000
	maxRetryTimes    = 5
	maxMsgSize       = 20*1024*1024
)

var (
	timeoutSeconds       int
	periodMillis         int
	url                  string

	waitCmd = &cobra.Command{
		Use:   "wait",
		Short: "Waits until the hsm sds server is ready",
		RunE: func(c *cobra.Command, args []string) error {
			log.Info("Waiting for hsm sds server to be ready (timeout:" + strconv.Itoa(timeoutSeconds) + " seconds)...")
			ctx := context.Background()
			var err error
			timeout := time.After(time.Duration(timeoutSeconds) * time.Second)

			for {
				select {
				case <-timeout:
					return fmt.Errorf("timeout waiting for mTLS SDS server to become ready. Last error: %v", err)
				case <-time.After(time.Duration(periodMillis) * time.Millisecond):
					log.Info("Need to wait SDS server to become ready!")
					// SDS server checking process
					mTLSExists, mTLSErr := checkSocket(ctx, security.WorkloadIdentitySocketPath)
					if mTLSErr != nil {
						log.Info("Not ready yet for mTLS SDS server error: ", mTLSErr)
						err = mTLSErr
						continue
					}
					if mTLSExists {
						log.Infof("UDS file %s found, mTLS SDS server is ready!", security.WorkloadIdentitySocketPath)
						return nil
					}
				}
			}
		},
	}
)

// Checks whether the socket exists and is responsive.
// If it doesn't exist, returns (false, nil)
// If it exists and is NOT responsive, tries to delete the socket file.
// If it can be deleted, returns (false, nil).
// If it cannot be deleted, returns (false, error).
// Otherwise, returns (true, nil)
func checkSocket(ctx context.Context, socketPath string) (bool, error) {
	socketExists := socketFileExists(socketPath)
	if !socketExists {
		return false, nil
	}

	err := socketHealthCheck(ctx, socketPath)
	if err != nil {
		log.Infof("SDS socket detected but not healthy: %v", err)
		err = os.Remove(socketPath)
		if err != nil {
			return false, fmt.Errorf("existing SDS socket could not be removed: %v", err)
		}
		return false, nil
	}

	return true, nil
}

func socketFileExists(path string) bool {
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return true
	}
	return false
}

func socketHealthCheck(ctx context.Context, socketPath string) error {
	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(time.Second))
	defer cancel()

	conn, err := grpc.DialContext(ctx, fmt.Sprintf("unix:%s", socketPath),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.FailOnNonTempDialError(true),
		grpc.WithReturnConnectionError(),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize), grpc.MaxCallSendMsgSize(maxMsgSize)),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	return nil
}

func init() {
	waitCmd.PersistentFlags().IntVar(&timeoutSeconds, "timeoutSeconds", 60, "maximum number of seconds to wait for Envoy to be ready")
	waitCmd.PersistentFlags().IntVar(&periodMillis, "periodMillis", 500, "number of milliseconds to wait between attempts")
}