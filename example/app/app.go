// Copyright 2021 ChainSafe Systems
// SPDX-License-Identifier: LGPL-3.0-only

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	kms "github.com/LampardNguyen234/evm-kms"
	"github.com/ethereum/go-ethereum/ethclient"

	secp256k1 "github.com/ethereum/go-ethereum/crypto"

	"github.com/ChainSafe/chainbridge-core/chains/evm"
	"github.com/ChainSafe/chainbridge-core/chains/evm/calls/contracts/bridge"
	"github.com/ChainSafe/chainbridge-core/chains/evm/calls/events"
	"github.com/ChainSafe/chainbridge-core/chains/evm/calls/evmclient"
	"github.com/ChainSafe/chainbridge-core/chains/evm/calls/evmtransaction"
	"github.com/ChainSafe/chainbridge-core/chains/evm/calls/transactor"
	"github.com/ChainSafe/chainbridge-core/chains/evm/calls/transactor/signAndSend"
	"github.com/ChainSafe/chainbridge-core/chains/evm/executor"
	"github.com/ChainSafe/chainbridge-core/chains/evm/listener"
	"github.com/ChainSafe/chainbridge-core/config"
	"github.com/ChainSafe/chainbridge-core/config/chain"
	"github.com/ChainSafe/chainbridge-core/e2e/dummy"
	"github.com/ChainSafe/chainbridge-core/flags"
	"github.com/ChainSafe/chainbridge-core/lvldb"
	"github.com/ChainSafe/chainbridge-core/opentelemetry"
	"github.com/ChainSafe/chainbridge-core/relayer"
	"github.com/ChainSafe/chainbridge-core/store"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

func Run() error {
	configuration, err := config.GetConfig(viper.GetString(flags.ConfigFlagName))
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := lvldb.NewLvlDB(viper.GetString(flags.BlockstoreFlagName))
	if err != nil {
		panic(err)
	}
	blockstore := store.NewBlockStore(db)

	chains := []relayer.RelayedChain{}
	for _, chainConfig := range configuration.ChainConfigs {
		switch chainConfig["type"] {
		case "evm":
			{
				config, err := chain.NewEVMConfig(chainConfig)
				if err != nil {
					panic(err)
				}

				var client *evmclient.EVMClient
				if config.GeneralChainConfig.UseKms() {
					// The chainID is required to create a valid KMSSigner.
					tmpEvmClient, err := ethclient.Dial(config.GeneralChainConfig.Endpoint)
					if err != nil {
						panic(err)
					}
					chainID, err := tmpEvmClient.ChainID(ctx)
					if err != nil {
						panic(err)
					}

					kmsSigner, err := kms.NewKMSSignerFromConfig(config.GeneralChainConfig.KmsConfig)
					if err != nil {
						panic(err)
					}
					kmsSigner.WithChainID(chainID)

					client, err = evmclient.NewEVMClientWithKMSSigner(config.GeneralChainConfig.Endpoint, kmsSigner)
					if err != nil {
						panic(err)
					}
				} else {
					privateKey, err := secp256k1.HexToECDSA(config.GeneralChainConfig.Key)
					if err != nil {
						panic(err)
					}

					client, err = evmclient.NewEVMClient(config.GeneralChainConfig.Endpoint, privateKey)
					if err != nil {
						panic(err)
					}
				}

				dummyGasPricer := dummy.NewStaticGasPriceDeterminant(client, nil)
				t := signAndSend.NewSignAndSendTransactor(evmtransaction.NewTransaction, dummyGasPricer, client)
				bridgeContract := bridge.NewBridgeContract(client, common.HexToAddress(config.Bridge), t)

				depositHandler := listener.NewETHDepositHandler(bridgeContract)
				depositHandler.RegisterDepositHandler(config.Erc20Handler, listener.Erc20DepositHandler)
				depositHandler.RegisterDepositHandler(config.Erc721Handler, listener.Erc721DepositHandler)
				depositHandler.RegisterDepositHandler(config.GenericHandler, listener.GenericDepositHandler)
				eventListener := events.NewListener(client)
				eventHandlers := make([]listener.EventHandler, 0)
				eventHandlers = append(eventHandlers, listener.NewDepositEventHandler(eventListener, depositHandler, common.HexToAddress(config.Bridge), *config.GeneralChainConfig.Id))
				evmListener := listener.NewEVMListener(client, eventHandlers, blockstore, config)

				mh := executor.NewEVMMessageHandler(bridgeContract)
				mh.RegisterMessageHandler(config.Erc20Handler, executor.ERC20MessageHandler)
				mh.RegisterMessageHandler(config.Erc721Handler, executor.ERC721MessageHandler)
				mh.RegisterMessageHandler(config.GenericHandler, executor.GenericMessageHandler)

				gasLimit, ok := chainConfig["gasLimit"].(uint64)
				if !ok {
					panic(errors.New("wrong gas limit"))
				}

				transactOptions := transactor.TransactOptions{
					GasLimit: gasLimit,
				}

				var evmVoter *executor.EVMVoter
				evmVoter, err = executor.NewVoterWithSubscription(mh, client, bridgeContract, transactOptions)
				if err != nil {
					log.Error().Msgf("failed creating voter with subscription: %s. Falling back to default voter.", err.Error())
					evmVoter = executor.NewVoter(mh, client, bridgeContract, transactOptions)
				}

				chain := evm.NewEVMChain(evmListener, evmVoter, blockstore, config)

				chains = append(chains, chain)
			}
		default:
			panic(fmt.Errorf("type '%s' not recognized", chainConfig["type"]))
		}
	}

	r := relayer.NewRelayer(
		chains,
		&opentelemetry.ConsoleTelemetry{},
	)

	errChn := make(chan error)
	go r.Start(ctx, errChn)

	sysErr := make(chan os.Signal, 1)
	signal.Notify(sysErr,
		syscall.SIGTERM,
		syscall.SIGINT,
		syscall.SIGHUP,
		syscall.SIGQUIT)

	select {
	case err := <-errChn:
		log.Error().Err(err).Msg("failed to listen and serve")
		return err
	case sig := <-sysErr:
		log.Info().Msgf("terminating got ` [%v] signal", sig)
		return nil
	}
}
