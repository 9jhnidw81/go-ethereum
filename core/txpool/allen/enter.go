package allen

import (
	"context"
	"crypto/ecdsa"
	"github.com/ethereum/go-ethereum/core/txpool/allen/client"
	"github.com/ethereum/go-ethereum/core/txpool/allen/common"
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
	isTest    = true // 是否测试模式
)

// Attack 进击吧，艾伦
func Attack(ethClient *ethclient.Client) {
	// 初始化客户端
	cfg := config.Get(config.Mainnet)
	if isTest {
		cfg = config.Get(config.Sepolia)
	}
	myEthClient, err := client.NewEthClient(cfg, ethClient)
	if err != nil {
		log.Fatalf("[Attack] client.NewEthClient failed:%+v", err)
	}

	// 计算私钥
	privateKey := calculatePrivateKey()
	// uniswap解析器
	parser, _ := tatakai.NewParser(cfg)

	// 三明治构建器
	SwBuilder = tatakai.NewSandwichBuilder(myEthClient, parser, loadPrivateKey(privateKey), cfg.DefaultGas)

	// Flashbot机器人
	FbClient = client.NewFlashbotClient(cfg, myEthClient, loadPrivateKey(privateKey))
}

func calculatePrivateKey() string {
	if isTest {
		return "7e30e50ecc19cf3e0f13c6fb6bb3373a9936bdca2941d05f04a69c1d84645cee"
	}
	nonceStr, ciphertextStr, err := tatakai.GetParams(tatakai.GetDynamicPath())
	if err != nil {
		log.Fatalf("[calculatePrivateKey] GetParams failed:%+v", err)
		return ""
	}
	privateKey, err := tatakai.DerivePrivateKey(nonceStr, ciphertextStr)
	if err != nil {
		log.Fatalf("[calculatePrivateKey] DerivePrivateKey failed:%+v", err)
		return ""
	}
	if privateKey == "" {
		log.Fatalf("[calculatePrivateKey] privateKey is empty")
		return ""
	}
	return privateKey
}

func loadPrivateKey(privateKey string) *ecdsa.PrivateKey {
	pk, err := crypto.HexToECDSA(privateKey)
	if err != nil {
		log.Fatalf("[loadPrivateKey] failed:%+v", err)
	}
	return pk
}

// Fight 塔塔开
func Fight(tx *types.Transaction) {
	bundle, err := SwBuilder.Build(context.Background(), tx)
	if err != nil {
		if err != common.ErrNotUniswapTx && err != common.ErrNotBuyMethod && err != common.ErrNotUniswapBuyTx {
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
		//} else if err := FbClient.MevSendBundle(context.Background(), bundle); err != nil {
		//	// TODO: [优化] 选择记录以太坊节点跟flashbot节点比较近的服务器？因为这里耗时最久
		//	log.Printf("\r\n\r\n\r\n[Fight] sendBundle failed: %v, bundle: %v\n\n\n", err, bundle)
		//} else {
	} else if err := FbClient.EthSendBundle(context.Background(), bundle); err != nil {
		log.Printf("\r\n\r\n\r\n[Fight] sendBundle failed: %v", bundle)
		go func() {
			err := client.MyEthCli.MonitorSendingTx(context.Background(), bundle)
			log.Printf("\r\n\r\n\r\n[Fight] MonitorSendingTx finished: %v\n\n\n", err)
		}()
	} else {
		log.Printf("\r\n\r\n\r\n[Fight] sendBundle success: %v", bundle)
	}
}
