package client

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/core/types"
	"log"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	// 最大等待区块，超过则重置nonce且可能需要加速？
	maxWaitingBlock = 5
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

	// 实时查询链上最新nonce
	currentChainNonce, err := c.PendingNonceAt(ctx, address)
	if err != nil {
		return 0, err
	}

	// 自动修正本地nonce
	local := atomic.LoadUint64(&c.localNonce)
	if local < currentChainNonce {
		atomic.StoreUint64(&c.localNonce, currentChainNonce)
		local = currentChainNonce
		log.Printf("[Nonce] 检测到链上nonce更新，本地已同步至%d", local)
	}

	// 分配当前nonce并递增
	newNonce := local
	atomic.StoreUint64(&c.localNonce, newNonce+1)

	log.Printf("[Nonce] 分配nonce %d (链上:%d)", newNonce, currentChainNonce)
	return newNonce, nil
}

// ForceSyncNonce 强制同步到链上最新状态
func (c *EthClient) ForceSyncNonce(ctx context.Context, address common.Address) error {
	c.nonceLock.Lock()
	defer c.nonceLock.Unlock()

	current, err := c.PendingNonceAt(ctx, address)
	if err != nil {
		return fmt.Errorf("强制同步失败: %v", err)
	}

	old := atomic.LoadUint64(&c.localNonce)
	atomic.StoreUint64(&c.localNonce, current)

	log.Printf("[Nonce] 强制同步完成：%d -> %d", old, current)
	return nil
}

// MonitorSendingTx 已发送交易监听
func (c *EthClient) MonitorSendingTx(ctx context.Context, address common.Address) error {
	c.nonceLock.Lock()
	defer c.nonceLock.Unlock()

	current, err := c.PendingNonceAt(ctx, address)
	if err != nil {
		return fmt.Errorf("强制同步失败: %v", err)
	}

	old := atomic.LoadUint64(&c.localNonce)
	atomic.StoreUint64(&c.localNonce, current)

	log.Printf("[Nonce] 强制同步完成：%d -> %d", old, current)
	return nil
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
