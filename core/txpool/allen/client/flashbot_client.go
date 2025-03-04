package client

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/metachris/flashbotsrpc"
)

type FlashbotClient struct {
	rpc        *flashbotsrpc.FlashbotsRPC
	ethClient  *EthClient
	config     *config.Config
	privateKey *ecdsa.PrivateKey
}

func NewFlashbotClient(cfg *config.Config, ethClient *EthClient, pk *ecdsa.PrivateKey) *FlashbotClient {
	return &FlashbotClient{
		rpc:        flashbotsrpc.New(cfg.FlashbotsEndpoint),
		ethClient:  ethClient,
		config:     cfg,
		privateKey: pk,
	}
}

func (c *FlashbotClient) SendBundle(ctx context.Context, txs []*types.Transaction) error {
	rawTxs := make([]string, 0, len(txs))
	for _, tx := range txs {
		data, err := tx.MarshalBinary()
		if err != nil {
			return err
		}
		rawTxs = append(rawTxs, "0x"+hex.EncodeToString(data))
	}

	// 使用以太坊客户端获取区块号
	blockNumber, err := c.ethClient.BlockNumber(ctx)
	if err != nil {
		return err
	}
	// 多中继，提高成功率
	//    "https://relay.flashbots.net",
	//    "https://bloxroute.ethical",
	//    "https://rsync-builder.xyz"
	fmt.Println("bln", blockNumber)
	_, err = c.rpc.FlashbotsSendBundle(c.privateKey, flashbotsrpc.FlashbotsSendBundleRequest{
		Txs:         rawTxs,
		BlockNumber: fmt.Sprintf("0x%x", blockNumber+1),
	})
	return err
}

func (c *FlashbotClient) CallBundle(ctx context.Context, txs []*types.Transaction) error {
	rawTxs := make([]string, 0, len(txs))
	for _, tx := range txs {
		data, err := tx.MarshalBinary()
		if err != nil {
			return err
		}
		rawTxs = append(rawTxs, "0x"+hex.EncodeToString(data))
	}

	// 使用以太坊客户端获取区块号
	blockNumber, err := c.ethClient.BlockNumber(ctx)
	if err != nil {
		return err
	}

	gasPrice, err := c.ethClient.SuggestGasPrice(ctx)
	if err != nil {
		return err
	}

	_, err = c.rpc.FlashbotsCallBundle(c.privateKey, flashbotsrpc.FlashbotsCallBundleParam{
		Txs:              rawTxs,
		BlockNumber:      fmt.Sprintf("0x%x", blockNumber+1),
		StateBlockNumber: "latest",
		BaseFee:          gasPrice.Uint64() + 508037861,
	})
	return err
}
