package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"golang.org/x/sync/errgroup"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/metachris/flashbotsrpc"
)

const (
	j               = "application/json"
	stats           = "flashbots_getUserStats"
	flashbotXHeader = "X-Flashbots-Signature"
	p               = "POST"
)

type FlashbotClient struct {
	rpc        *flashbotsrpc.FlashbotsRPC
	EthClient  *EthClient
	config     *config.Config
	privateKey *ecdsa.PrivateKey
}

// MevSendBundleRequest 完整文档结构定义
type MevSendBundleRequest struct {
	JsonRPC string               `json:"jsonrpc"`
	ID      string               `json:"id"`
	Method  string               `json:"method"`
	Params  []MevSendBundleParam `json:"params"`
}

// EthSendBundleRequest 完整文档结构定义
type EthSendBundleRequest struct {
	JsonRPC string               `json:"jsonrpc"`
	ID      string               `json:"id"`
	Method  string               `json:"method"`
	Params  []EthSendBundleParam `json:"params"`
}

type MevSendBundleParam struct {
	Version   string     `json:"version"`
	Inclusion Inclusion  `json:"inclusion"`
	Body      []BodyItem `json:"body"`
	Validity  *Validity  `json:"validity,omitempty"`
	Privacy   *Privacy   `json:"privacy,omitempty"`
}

type EthSendBundleParam struct {
	Txs         []string `json:"txs"`
	BlockNumber string   `json:"blockNumber"`
	Builders    []string `json:"builders,omitempty"`
}

type Inclusion struct {
	Block    string  `json:"block"`
	MaxBlock *string `json:"maxBlock,omitempty"`
}

type BodyItem struct {
	Hash      *string             `json:"hash,omitempty"`
	Tx        *string             `json:"tx,omitempty"`
	CanRevert *bool               `json:"canRevert,omitempty"`
	Bundle    *MevSendBundleParam `json:"bundle,omitempty"`
}

type Validity struct {
	Refund       []Refund       `json:"refund,omitempty"`
	RefundConfig []RefundConfig `json:"refundConfig,omitempty"`
}

type Refund struct {
	BodyIdx int `json:"bodyIdx"`
	Percent int `json:"percent"`
}

type RefundConfig struct {
	Address string `json:"address"`
	Percent int    `json:"percent"`
}

type Privacy struct {
	Hints    []string `json:"hints,omitempty"`
	Builders []string `json:"builders,omitempty"`
}

func NewFlashbotClient(cfg *config.Config, ethClient *EthClient, pk *ecdsa.PrivateKey) *FlashbotClient {
	return &FlashbotClient{
		rpc:        flashbotsrpc.New(cfg.FlashbotsEndpoint[0]), // 这里主要是mock交易，只需要一条节点即可
		EthClient:  ethClient,
		config:     cfg,
		privateKey: pk,
	}
}

func (c *FlashbotClient) MevSendBundle(ctx context.Context, txs []*types.Transaction) error {
	const (
		methodPrefix = "MevSendBundle"
	)
	bundleParam, err := c.buildMevBundleParam(ctx, txs)
	if err != nil {
		return err
	}
	mevHTTPClient := &http.Client{
		Timeout: time.Second * 5,
	}

	param := MevSendBundleRequest{
		JsonRPC: "2.0",
		ID:      "1",
		Method:  "mev_sendBundle",
		Params:  []MevSendBundleParam{*bundleParam},
	}
	payload, _ := json.Marshal(param)

	var (
		eg errgroup.Group
	)

	endPoints := c.config.FlashbotsEndpoint
	for i := 0; i < len(endPoints); i++ {
		eg.Go(func() error {
			req, _ := http.NewRequest("POST", endPoints[i], bytes.NewBuffer(payload))
			headerReady, _ := crypto.Sign(
				accounts.TextHash([]byte(hexutil.Encode(crypto.Keccak256(payload)))),
				c.privateKey,
			)
			req.Header.Add("content-type", j)
			req.Header.Add("Accept", j)
			req.Header.Add(flashbotXHeader, flashbotHeader(headerReady, c.privateKey))
			resp, err := mevHTTPClient.Do(req)
			if err != nil {
				return fmt.Errorf("[%s] do request failed:%w", methodPrefix, err)
			}
			res, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("[%s] read all body(%+v) failed:%w", methodPrefix, resp.Body, err)
			}
			fmt.Println("Mev_sendBundle")
			fmt.Println(string(res))
			return nil
		})
	}

	return nil
}

func (c *FlashbotClient) EthSendBundle(ctx context.Context, txs []*types.Transaction) error {
	const (
		methodPrefix = "EthSendBundle"
	)

	bundleParam, err := c.buildEthBundleParam(ctx, txs)
	if err != nil {
		return err
	}
	mevHTTPClient := &http.Client{
		Timeout: time.Second * 5,
	}

	param := EthSendBundleRequest{
		JsonRPC: "2.0",
		ID:      "1",
		Method:  "eth_sendBundle",
		Params:  []EthSendBundleParam{*bundleParam},
	}
	payload, _ := json.Marshal(param)

	var (
		eg errgroup.Group
	)

	endPoints := c.config.FlashbotsEndpoint
	for i := 0; i < len(endPoints); i++ {
		eg.Go(func() error {
			req, _ := http.NewRequest("POST", endPoints[i], bytes.NewBuffer(payload))
			headerReady, _ := crypto.Sign(
				accounts.TextHash([]byte(hexutil.Encode(crypto.Keccak256(payload)))),
				c.privateKey,
			)
			req.Header.Add("content-type", j)
			req.Header.Add("Accept", j)
			req.Header.Add(flashbotXHeader, flashbotHeader(headerReady, c.privateKey))
			resp, err := mevHTTPClient.Do(req)
			if err != nil {
				log.Info(methodPrefix, "-", endPoints[i], "do request failed", err)
				return nil
				//return fmt.Errorf("[%s] do request failed:%w", methodPrefix, err)
			}
			res, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				log.Info(methodPrefix, "-", endPoints[i], "read all body", resp.Body, " failed", err)
				return nil
				//return fmt.Errorf("[%s] read all body(%+v) failed:%w", methodPrefix, resp.Body, err)
			}
			fmt.Printf("Eth_sendBundle-%s-%s\n", endPoints[i], string(res))
			return nil
		})
	}

	if err = eg.Wait(); err != nil {
		return err
	}

	return nil
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
	blockNumber, err := c.EthClient.BlockNumber(ctx)
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
	blockNumber, err := c.EthClient.BlockNumber(ctx)
	if err != nil {
		return err
	}

	gasPrice, err := c.EthClient.SuggestGasPrice(ctx)
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

func (c *FlashbotClient) buildMevBundleParam(ctx context.Context, txs []*types.Transaction) (*MevSendBundleParam, error) {
	// 构建交易参数
	bodyItems := make([]BodyItem, 0, len(txs))
	for _, tx := range txs {
		data, err := tx.MarshalBinary()
		if err != nil {
			return nil, err
		}
		bodyItems = append(bodyItems, BodyItem{
			Tx:        ptrString("0x" + hex.EncodeToString(data)),
			CanRevert: ptrBool(false),
		})
	}

	// 设置区块包含范围（下两个区块）
	currentBlock, _ := c.EthClient.BlockNumber(ctx)
	targetBlock := fmt.Sprintf("0x%x", currentBlock+1)
	maxBlock := fmt.Sprintf("0x%x", currentBlock+3)

	// 构建完整Bundle参数
	bundle := MevSendBundleParam{
		Version: "v0.1",
		//Version: "v0.2",
		Inclusion: Inclusion{
			Block:    targetBlock,
			MaxBlock: &maxBlock,
		},
		Validity: &Validity{
			RefundConfig: []RefundConfig{
				{Address: crypto.PubkeyToAddress(c.privateKey.PublicKey).Hex(), Percent: 100}, // 自己占有所有收益
			},
		},
		//Privacy: &Privacy{
		//	Hints:    []string{"calldata", "hash"},                                                      // 最小暴露
		//	Builders: []string{"flashbots", "bloxroute", "builder0x69", "rsync-builder", "beaverbuild"}, // 全量 builders
		//},
		// 修改后（测试网专用）
		Privacy: &Privacy{
			Hints:    []string{"hash"}, // 最小化信息暴露
			Builders: c.config.Builders,
			//Builders: []string{"flashbots", "builder0x69-testnet"},
		},
		Body: bodyItems,
	}
	return &bundle, nil
}

func (c *FlashbotClient) buildEthBundleParam(ctx context.Context, txs []*types.Transaction) (*EthSendBundleParam, error) {
	// 构建交易参数
	rawTxs := make([]string, 0, len(txs))
	for _, tx := range txs {
		data, err := tx.MarshalBinary()
		if err != nil {
			return nil, err
		}
		rawTxs = append(rawTxs, "0x"+hex.EncodeToString(data))
	}

	// 设置区块
	currentBlock, _ := c.EthClient.BlockNumber(ctx)
	targetBlock := fmt.Sprintf("0x%x", currentBlock+1)

	// 构建完整Bundle参数
	bundle := EthSendBundleParam{
		Txs:         rawTxs,
		BlockNumber: targetBlock,
		Builders:    c.config.Builders,
	}
	return &bundle, nil
}

// 辅助函数
func ptrString(s string) *string { return &s }
func ptrBool(b bool) *bool       { return &b }

func flashbotHeader(signature []byte, privateKey *ecdsa.PrivateKey) string {
	return crypto.PubkeyToAddress(privateKey.PublicKey).Hex() + ":" + hexutil.Encode(signature)
}
