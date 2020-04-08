/*
Copyright © 2020 Celo Org

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/celo-org/rosetta/celo"
	"github.com/celo-org/rosetta/celo/client"
	"github.com/celo-org/rosetta/db"
	"github.com/celo-org/rosetta/internal/fileutils"
	"github.com/celo-org/rosetta/internal/signals"
	"github.com/celo-org/rosetta/service"
	"github.com/celo-org/rosetta/service/geth"
	"github.com/celo-org/rosetta/service/monitor"
	"github.com/celo-org/rosetta/service/rpc"
	"github.com/ethereum/go-ethereum/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "run",
	Short: "Start rosetta server",
	Args:  cobra.NoArgs,
	Run:   runRunCmd,
}

var rosettaRpcConfig rpc.RosettaServerConfig

type ConfigPaths string

var gethBinary string
var staticNodes []string

func init() {
	rootCmd.AddCommand(serveCmd)

	flagSet := serveCmd.Flags()

	// Common Flags
	flagSet.String("datadir", "", "datadir to use")
	exitOnError(viper.BindPFlag("datadir", flagSet.Lookup("datadir")))
	exitOnError(serveCmd.MarkFlagDirname("datadir"))

	// RPC Service Flags
	flagSet.UintVar(&rosettaRpcConfig.Port, "port", 8080, "Listening port for http server")
	flagSet.StringVar(&rosettaRpcConfig.Interface, "address", "", "Listening address for http server")
	flagSet.DurationVar(&rosettaRpcConfig.RequestTimeout, "reqTimeout", 25*time.Second, "Timeout when serving a request")

	// Geth Service Flags
	flagSet.String("geth", "", "Path to the celo-blockchain binary")
	exitOnError(viper.BindPFlag("geth", flagSet.Lookup("geth")))
	exitOnError(serveCmd.MarkFlagFilename("geth"))

	flagSet.String("genesis", "", "path to the genesis.json")
	exitOnError(viper.BindPFlag("genesis", flagSet.Lookup("genesis")))
	exitOnError(serveCmd.MarkFlagFilename("genesis", "json"))

	flagSet.StringArrayVar(&staticNodes, "staticNode", []string{}, "StaticNode to use (can be repeated many times")
	exitOnError(serveCmd.MarkFlagRequired("staticNode"))

	// Monitor Service Flags

}

func getDatadir(cmd *cobra.Command) string {
	exitOnMissingConfig(cmd, "datadir")

	absDatadir, err := filepath.Abs(viper.GetString("datadir"))
	if err != nil {
		log.Crit("Can't resolve datadir path", "datadir", absDatadir, "err", err)
	}

	isDir, err := fileutils.IsDirectory(absDatadir)
	if err != nil {
		log.Crit("Can't access datadir", "datadir", absDatadir, "err", err)
	} else if !isDir {
		log.Crit("Datadir is not a directory", "datadir", absDatadir)
	}

	return absDatadir
}

func rosettaServiceFactory(chainParams *celo.ChainParameters, nodeUri string, db db.RosettaDB) service.ServieFactory {
	return func() (service.Service, error) {

		cc, err := client.Dial(nodeUri)
		if err != nil {
			return nil, fmt.Errorf("Error on client connection to geth: %w", err)
		}

		rosettaServerSrv := rpc.NewRosettaServer(cc, &rosettaRpcConfig, chainParams, db)
		monitorSrv := monitor.NewMonitorService(cc, db)

		return service.Group(rosettaServerSrv, monitorSrv), nil
	}
}

func runRunCmd(cmd *cobra.Command, args []string) {
	datadir := getDatadir(cmd)
	gethDataDir := filepath.Join(datadir, "celo")
	sqlitePath := filepath.Join(datadir, "rosetta.db")

	exitOnMissingConfig(cmd, "geth")
	exitOnMissingConfig(cmd, "genesis")

	gethBinary = viper.GetString("geth")
	genesisPath := viper.GetString("genesis")

	gethSrv := geth.NewGethService(
		gethBinary,
		gethDataDir,
		genesisPath,
		staticNodes,
	)

	if err := gethSrv.Setup(); err != nil {
		log.Error("Error on geth setup", "err", err)
		os.Exit(1)
	}

	chainParams := gethSrv.ChainParameters()
	log.Info("Detected Chain Parameters", "chainId", chainParams.ChainId, "epochSize", chainParams.EpochSize)

	nodeUri := gethSrv.IpcFilePath()
	log.Debug("celo nodes ipc file", "filepath", nodeUri)

	celoStore, err := db.NewSqliteDb(sqlitePath)
	if err != nil {
		log.Error("Error opening CeloStore", "err", err)
		os.Exit(1)
	}

	delayedServices := service.WithDelay(service.LazyService("rosetta", rosettaServiceFactory(chainParams, nodeUri, celoStore)), 5*time.Second)

	// TODO - create context that encapsulate Stop on Signal behaviour
	srvCtx, stopServices := context.WithCancel(context.Background())
	defer stopServices()

	gotExitSignal := signals.WatchForExitSignals()
	go func() {
		<-gotExitSignal
		stopServices()
	}()

	if err := service.RunServices(srvCtx, gethSrv, delayedServices); err != nil {
		log.Error("Error running services", "err", err)
		os.Exit(1)
	}
}
