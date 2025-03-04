package client

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/core/types"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/ethclient"
)

type EthClient struct {
	*ethclient.Client
	Config *config.Config
	// 原子操作维护的本地nonce
	localNonce uint64
	nonceLock  sync.Mutex
}

func NewEthClient(cfg *config.Config, client *ethclient.Client) (*EthClient, error) {
	return &EthClient{
		Client:     client,       // 继承的ethclient
		Config:     cfg,          // 配置信息
		localNonce: 0,            // 显式初始化本地nonce
		nonceLock:  sync.Mutex{}, // 显式初始化互斥锁
	}, nil
}

// GetDynamicGasPrice 线上直接通过自建节点获取，无需通过三方节点
func (c *EthClient) GetDynamicGasPrice(ctx context.Context) (*big.Int, error) {
	suggested, err := c.SuggestGasPrice(ctx)
	if err != nil {
		return nil, err
	}

	maxGas := big.NewInt(c.Config.GasConfig.MaxGasGwei * 1e9)
	adjusted := multiplyBigInt(suggested, c.Config.GasConfig.BaseMultiplier)

	if adjusted.Cmp(maxGas) > 0 {
		return maxGas, nil
	}
	return adjusted, nil
}

// GetSequentialNonce 获取nonce
func (c *EthClient) GetSequentialNonce(ctx context.Context, address common.Address) (uint64, error) {
	c.nonceLock.Lock()
	defer c.nonceLock.Unlock()

	// 第一次获取时初始化
	if atomic.LoadUint64(&c.localNonce) == 0 {
		pendingNonce, err := c.PendingNonceAt(ctx, address)
		if err != nil {
			return 0, err
		}
		atomic.StoreUint64(&c.localNonce, pendingNonce)
	}

	// 获取并递增
	current := atomic.LoadUint64(&c.localNonce)
	atomic.AddUint64(&c.localNonce, 1)
	return current, nil
}

// SyncNonce 同步链上nonce的
func (c *EthClient) SyncNonce(ctx context.Context, address common.Address) error {
	c.nonceLock.Lock()
	defer c.nonceLock.Unlock()

	pending, err := c.PendingNonceAt(ctx, address)
	if err != nil {
		return err
	}
	atomic.StoreUint64(&c.localNonce, pending)
	return nil
}

// AccelerateTransaction 发送加速交易覆盖指定nonce
// 参数说明：
// - ctx: 上下文
// - privateKey: 账户私钥
// - fromAddress: 发送地址（必须与私钥对应）
// - targetNonce: 需要覆盖的nonce
// - gasMultiplier: 原交易gas价格的倍数（建议1.5-2倍）
func (c *EthClient) AccelerateTransaction(
	ctx context.Context,
	privateKey *ecdsa.PrivateKey,
	fromAddress common.Address,
	targetNonce uint64,
	gasMultiplier float64,
) (*types.Transaction, error) {
	var (
		methodPrefix = "AccelerateTransaction"
	)

	// 获取当前链上nonce
	currentNonce, err := c.PendingNonceAt(ctx, fromAddress)
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取链上nonce失败: %w", methodPrefix, err)
	}

	// 验证nonce有效性
	if targetNonce < currentNonce {
		return nil, fmt.Errorf("[%s] nonce %d 已确认不可覆盖", methodPrefix, targetNonce)
	}

	// 计算加速gas价格
	baseGas, err := c.SuggestGasPrice(ctx)
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取基准gas价格失败: %w", methodPrefix, err)
	}
	acceleratedGas := new(big.Int).Mul(baseGas, big.NewInt(int64(gasMultiplier*100)))
	acceleratedGas.Div(acceleratedGas, big.NewInt(100))

	// 构造零值交易
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    targetNonce,
		To:       &fromAddress, // 发送给自己
		Value:    big.NewInt(0),
		Gas:      21000, // 基础gas limit
		GasPrice: acceleratedGas,
		Data:     nil,
	})

	// 签名交易
	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(c.Config.ChainID), privateKey)
	if err != nil {
		return nil, fmt.Errorf("[%s] 交易签名失败: %w", methodPrefix, err)
	}

	// 发送交易
	if err := c.SendTransaction(ctx, signedTx); err != nil {
		return nil, fmt.Errorf("[%s] 交易发送失败: %w", methodPrefix, err)
	}

	// 更新本地nonce状态
	c.nonceLock.Lock()
	defer c.nonceLock.Unlock()
	if atomic.LoadUint64(&c.localNonce) <= targetNonce {
		atomic.StoreUint64(&c.localNonce, targetNonce+1)
	}

	return signedTx, nil
}

func multiplyBigInt(val *big.Int, multiplier float64) *big.Int {
	f := new(big.Float).SetInt(val)
	f.Mul(f, big.NewFloat(multiplier))
	result, _ := f.Int(nil)
	return result
}
