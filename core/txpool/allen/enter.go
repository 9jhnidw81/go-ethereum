package allen

import (
	"context"
	"crypto/ecdsa"
	"github.com/ethereum/go-ethereum/core/txpool/allen/client"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/core/txpool/allen/tatakai"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"log"
)

var (
	SwBuilder *tatakai.SandwichBuilder
	FbClient  *client.FlashbotClient
)

// Attack 进击吧，艾伦
func Attack(ethClient *ethclient.Client) {
	// 初始化客户端
	cfg := config.Get(config.Sepolia)
	myEthClient, err := client.NewEthClient(cfg, ethClient)
	if err != nil {
		log.Fatalf("[Attack] client.NewEthClient failed:%+v", err)
	}

	// uniswap解析器
	parser, _ := tatakai.NewParser(cfg.RouterAddress, cfg.WETHAddress, cfg.FactoryAddress, cfg.RouterAbi, cfg.Erc20Abi, cfg.PairAbi)

	// 三明治构建器
	SwBuilder = tatakai.NewSandwichBuilder(myEthClient, parser, loadPrivateKey())

	// Flashbot机器人
	FbClient = client.NewFlashbotClient(cfg, myEthClient, loadPrivateKey())
}

// 测试号无所谓，只是为了跑流程
func loadPrivateKey() *ecdsa.PrivateKey {
	pk, err := crypto.HexToECDSA("7e30e50ecc19cf3e0f13c6fb6bb3373a9936bdca2941d05f04a69c1d84645cee")
	if err != nil {
		log.Fatal(err)
	}
	return pk
}

// Fight 塔塔开
func Fight(tx *types.Transaction) {
	bundle, err := SwBuilder.Build(context.Background(), tx)
	if err != nil {
		if err != tatakai.ErrNotUniswapTx && err != tatakai.ErrNotBuyMethod && err != tatakai.ErrNotUniswapBuyTx {
			log.Printf("[Fight] build failed: %v", err)
		}
		return
	}
	// 模拟交易
	// 提高gas
	// 三个nonce
	if err := FbClient.CallBundle(context.Background(), bundle); err != nil {
		// TODO： nonce too high(因为之前有三明治交易没有成功交易，导致本地的nonce一直在增加，使用更高的nonce导致交易被拒)
		// TODO: 还需要知道当前累计发送的nonce记录
		// 新增错误回滚
		_ = FbClient.EthClient.ForceSyncNonce(context.Background(), SwBuilder.FromAddress)
		log.Printf("\r\n\r\n\r\n[Fight] 模拟失败，已回滚nonce状态")
		log.Printf("\r\n\r\n\r\n[Fight] CallBundle failed: %v, bundle: %v\n\n\n", err, bundle)
	} else if err := FbClient.MevSendBundle(context.Background(), bundle); err != nil {
		log.Printf("\r\n\r\n\r\n[Fight] sendBundle failed: %v, bundle: %v\n\n\n", err, bundle)
	} else {
		log.Printf("\r\n\r\n\r\n[Fight] sendBundle success: %v\n\n\n", bundle)
	}
}
