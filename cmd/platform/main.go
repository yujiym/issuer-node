package main

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/polygonid/sh-id-platform/internal/api"
	"github.com/polygonid/sh-id-platform/internal/config"
	"github.com/polygonid/sh-id-platform/internal/core/ports"
	"github.com/polygonid/sh-id-platform/internal/core/services"
	"github.com/polygonid/sh-id-platform/internal/db"
	"github.com/polygonid/sh-id-platform/internal/gateways"
	"github.com/polygonid/sh-id-platform/internal/kms"
	"github.com/polygonid/sh-id-platform/internal/log"
	"github.com/polygonid/sh-id-platform/internal/providers"
	"github.com/polygonid/sh-id-platform/internal/providers/blockchain"
	"github.com/polygonid/sh-id-platform/internal/repositories"
	"github.com/polygonid/sh-id-platform/pkg/blockchain/eth"
	"github.com/polygonid/sh-id-platform/pkg/loaders"
	"github.com/polygonid/sh-id-platform/pkg/protocol"
	"github.com/polygonid/sh-id-platform/pkg/reverse_hash"
)

func main() {
	cfg, err := config.Load("")
	if err != nil {
		log.Error(context.Background(), "cannot load config", err)
		panic(err)
	}

	// Context with log
	ctx := log.NewContext(context.Background(), cfg.Log.Level, cfg.Log.Mode, os.Stdout)

	storage, err := db.NewStorage(cfg.Database.URL)
	if err != nil {
		log.Error(ctx, "cannot connect to database", err)
		panic(err)
	}

	vaultCli, err := providers.NewVaultClient(cfg.KeyStore.Address, cfg.KeyStore.Token)
	if err != nil {
		log.Error(ctx, "cannot init vault client: ", err)
		panic(err)
	}

	bjjKeyProvider, err := kms.NewVaultPluginIden3KeyProvider(vaultCli, cfg.KeyStore.PluginIden3MountPath, kms.KeyTypeBabyJubJub)
	if err != nil {
		log.Error(ctx, "cannot create BabyJubJub key provider: %+v", err)
		panic(err)
	}

	ethKeyProvider, err := kms.NewVaultPluginIden3KeyProvider(vaultCli, cfg.KeyStore.PluginIden3MountPath, kms.KeyTypeEthereum)
	if err != nil {
		log.Error(ctx, "cannot create Ethereum key provider", err)
		panic(err)
	}

	keyStore := kms.NewKMS()
	err = keyStore.RegisterKeyProvider(kms.KeyTypeBabyJubJub, bjjKeyProvider)
	if err != nil {
		log.Error(ctx, "cannot register BabyJubJub key provider: %+v", err)
		panic(err)
	}

	err = keyStore.RegisterKeyProvider(kms.KeyTypeEthereum, ethKeyProvider)
	if err != nil {
		log.Error(ctx, "cannot register Ethereum key provider", err)
		panic(err)
	}

	identityRepo := repositories.NewIdentity()
	claimsRepo := repositories.NewClaims()
	mtRepo := repositories.NewIdentityMerkleTreeRepository()
	identityStateRepo := repositories.NewIdentityState()
	revocationRepository := repositories.NewRevocation()
	mtService := services.NewIdentityMerkleTrees(mtRepo)

	rhsp := reverse_hash.NewRhsPublisher(nil, false)
	identityService := services.NewIdentity(keyStore, identityRepo, mtRepo, identityStateRepo, mtService, claimsRepo, revocationRepository, storage, rhsp)
	schemaService := services.NewSchema(storage)
	claimsService := services.NewClaim(
		claimsRepo,
		schemaService,
		identityService,
		mtService,
		identityStateRepo,
		storage,
		services.ClaimCfg{
			RHSEnabled: cfg.ReverseHashService.Enabled,
			RHSUrl:     cfg.ReverseHashService.URL,
			Host:       cfg.ServerUrl,
		},
	)

	stateContract, err := blockchain.InitEthClient(cfg.Ethereum.URL, cfg.Ethereum.ContractAddress)
	if err != nil {
		log.Error(ctx, "failed init ethereum client: %+v", err)
		panic(err)
	}

	ethConn, err := blockchain.InitEthConnect(cfg.Ethereum)
	if err != nil {
		log.Error(ctx, "failed init ethereum connect: %+v", err)
		panic(err)
	}

	revocationService := services.NewRevocationService(ethConn, common.HexToAddress(cfg.Ethereum.ContractAddress))
	zkProofService := services.NewProofService(claimsService, revocationService, schemaService, identityService, mtService, claimsRepo, keyStore, storage, stateContract)
	packageManager, err := protocol.InitPackageManager(ctx, stateContract, zkProofService, cfg.Circuit.Path)
	if err != nil {
		log.Error(ctx, "failed init package protocol:  %+v", err)
		panic(err)
	}

	commonClient, err := ethclient.Dial(cfg.Ethereum.URL)
	if err != nil {
		panic("Error dialing with ethclient: " + err.Error())
	}

	cl := eth.NewClient(commonClient, &eth.ClientConfig{
		DefaultGasLimit:        cfg.Ethereum.DefaultGasLimit,
		ConfirmationTimeout:    cfg.Ethereum.ConfirmationTimeout,
		ConfirmationBlockCount: cfg.Ethereum.ConfirmationBlockCount,
		ReceiptTimeout:         cfg.Ethereum.ReceiptTimeout,
		MinGasPrice:            big.NewInt(int64(cfg.Ethereum.MinGasPrice)),
		MaxGasPrice:            big.NewInt(int64(cfg.Ethereum.MaxGasPrice)),
		RPCResponseTimeout:     cfg.Ethereum.RPCResponseTimeout,
		WaitReceiptCycleTime:   cfg.Ethereum.WaitReceiptCycleTime,
		WaitBlockCycleTime:     cfg.Ethereum.WaitBlockCycleTime,
	})
	publisherGateway, err := gateways.NewPublisherEthGateway(cl, common.HexToAddress(cfg.Ethereum.ContractAddress), keyStore, cfg.PublishingKeyPath)
	if err != nil {
		log.Error(ctx, "error creating publish gateway", err)
		panic("error creating publish gateway")
	}

	circuitsLoaderService := loaders.NewCircuits(cfg.Circuit.Path)
	proofService := initProofService(cfg, circuitsLoaderService)
	transactionService, err := gateways.NewTransaction(cl, cfg.Ethereum.ConfirmationBlockCount)
	if err != nil {
		log.Error(ctx, "error creating transaction service", err)
		panic("error creating transaction service")
	}

	publisher := gateways.NewPublisher(storage, identityService, claimsService, mtService, keyStore, transactionService, proofService, publisherGateway, cfg.Ethereum.ConfirmationTimeout)

	spec, err := api.GetSwagger()
	if err != nil {
		log.Error(ctx, "cannot retrieve the openapi specification file: %+v", err)
		os.Exit(1)
	}
	spec.Servers = nil
	mux := chi.NewRouter()
	mux.Use(
		chiMiddleware.RequestID,
		log.ChiMiddleware(ctx),
		chiMiddleware.Recoverer,
		cors.Handler(cors.Options{AllowedOrigins: []string{"*"}}),
	)
	api.HandlerFromMux(
		api.NewStrictHandlerWithOptions(
			api.NewServer(cfg, identityService, claimsService, schemaService, publisher, packageManager),
			middlewares(ctx, cfg.HTTPBasicAuth),
			api.StrictHTTPServerOptions{
				RequestErrorHandlerFunc:  api.RequestErrorHandlerFunc,
				ResponseErrorHandlerFunc: api.ResponseErrorHandlerFunc,
			}),
		mux)
	api.RegisterStatic(mux)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.ServerPort),
		Handler: mux,
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Info(ctx, fmt.Sprintf("server started on port:%d", cfg.ServerPort))
		if err := server.ListenAndServe(); err != nil {
			log.Error(ctx, "Starting http server", err)
		}
	}()

	<-quit
	log.Info(ctx, "Shutting down")
}

func middlewares(ctx context.Context, auth config.HTTPBasicAuth) []api.StrictMiddlewareFunc {
	return []api.StrictMiddlewareFunc{
		api.LogMiddleware(ctx),
		api.BasicAuthMiddleware(ctx, auth.User, auth.Password),
	}
}

func initProofService(config *config.Configuration, circuitLoaderService *loaders.Circuits) ports.ZKGenerator {
	log.Info(context.Background(), fmt.Sprintf("native prover enabled: %v", config.NativeProofGenerationEnabled))
	if config.NativeProofGenerationEnabled {
		proverConfig := &services.NativeProverConfig{
			CircuitsLoader: circuitLoaderService,
		}
		return services.NewNativeProverService(proverConfig)
	}

	proverConfig := &gateways.ProverConfig{
		ServerURL:       config.Prover.ServerURL,
		ResponseTimeout: config.Prover.ResponseTimeout,
	}

	return gateways.NewProverService(proverConfig)
}
