package client

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	common2 "github.com/ethereum/go-ethereum/core/txpool/allen/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/core/types"
	"log"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	MyEthCli *EthClient
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
	// 原子变量记录首笔交易发送时的区块
	firstTxBlock uint64
}

func NewEthClient(cfg *config.Config, client *ethclient.Client) (*EthClient, error) {
	MyEthCli = &EthClient{
		Client:     client,       // 继承的ethclient
		Config:     cfg,          // 配置信息
		localNonce: 0,            // 显式初始化本地nonce
		nonceLock:  sync.Mutex{}, // 显式初始化互斥锁
	}
	return MyEthCli, nil
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

// ForceSyncNonce 强制同步到链上最新状态
func (c *EthClient) ForceSyncNonce(ctx context.Context, address common.Address) error {
	const (
		methodPrefix = "ForceSyncNonce"
	)
	c.nonceLock.Lock()
	defer c.nonceLock.Unlock()

	current, err := c.PendingNonceAt(ctx, address)
	if err != nil {
		return fmt.Errorf("[%s]强制同步失败: %v", methodPrefix, err)
	}

	//old := atomic.LoadUint64(&c.localNonce)
	atomic.StoreUint64(&c.localNonce, current)

	//log.Printf("[Nonce] 强制同步完成：%d -> %d", old, current)
	return nil
}

// MonitorSendingTx 已发送交易监听，txs是三个交易的列表
// 需要判断第一条交易是否超过5个区块，超过认为已经失败，调用ForceSyncNonce()
func (c *EthClient) MonitorSendingTx(ctx context.Context, txs []*types.Transaction) error {
	const (
		methodPrefix  = "MonitorSendingTx"
		checkInterval = 12 * time.Second // 区块时间按12秒估算
		maxAttempts   = 10               // 最大尝试次数（约6分钟）
	)

	if len(txs) == 0 {
		return errors.New("空交易列表")
	}

	firstTx := txs[0]
	fromAddress, err := types.Sender(types.NewEIP155Signer(c.Config.ChainID), firstTx)
	if err != nil {
		return fmt.Errorf("[%s] 解析发送地址失败: %w", methodPrefix, err)
	}

	var (
		lastCheckedBlock uint64
		attempt          int
	)

	for attempt = 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// 获取当前区块高度
			currentBlock, err := c.BlockNumber(ctx)
			if err != nil {
				log.Printf("[Monitor] 获取区块高度失败: %v (重试 %d/%d)", err, attempt+1, maxAttempts)
				time.Sleep(checkInterval)
				continue
			}

			// 避免重复检查相同区块
			if currentBlock <= lastCheckedBlock {
				time.Sleep(checkInterval)
				continue
			}
			lastCheckedBlock = currentBlock

			// 检查首笔交易状态
			receipt, err := c.TransactionReceipt(ctx, firstTx.Hash())
			if err == ethereum.NotFound {
				// 交易尚未被打包，检查区块延迟
				startBlock := atomic.LoadUint64(&c.firstTxBlock)
				if startBlock == 0 {
					// 记录交易发送时的初始区块
					atomic.StoreUint64(&c.firstTxBlock, currentBlock)
					startBlock = currentBlock
				}

				elapsedBlocks := currentBlock - startBlock
				if elapsedBlocks >= maxWaitingBlock {
					log.Printf("[Monitor] 交易 %s 超过 %d 个区块未确认，强制重置nonce",
						firstTx.Hash().Hex(), maxWaitingBlock)
					if syncErr := c.ForceSyncNonce(ctx, fromAddress); syncErr != nil {
						return fmt.Errorf("强制同步nonce失败: %v (原始错误: 交易未确认)", syncErr)
					}
					return common2.ErrTxStuck
				}

				log.Printf("[Monitor] 交易等待中: 已等待 %d/%d 个区块",
					elapsedBlocks, maxWaitingBlock)
				time.Sleep(checkInterval)
				continue
			} else if err != nil {
				log.Printf("[Monitor] 获取交易回执失败: %v (重试 %d/%d)", err, attempt+1, maxAttempts)
				time.Sleep(checkInterval)
				continue
			}

			// 交易已确认，检查后续交易
			if receipt.BlockNumber.Uint64() == 0 {
				continue
			}

			log.Printf("[Monitor-Fight] 首笔交易 %s 已在区块 %d 确认",
				firstTx.Hash().Hex(), receipt.BlockNumber.Uint64())
			return nil // 首笔交易成功，停止监控
		}
	}

	return fmt.Errorf("[%s] 监控超时，已达最大尝试次数 %d", methodPrefix, maxAttempts)
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

// SpeedNonce 发送加速交易覆盖指定nonce
// 参数说明：
// - ctx: 上下文
// - privateKey: 账户私钥
// - fromAddress: 发送地址（必须与私钥对应）
// - targetNonce: 需要覆盖的nonce
// - gasMultiplier: 原交易gas价格的倍数（建议1.5-2倍）
func (c *EthClient) SpeedNonce(
	ctx context.Context,
	privateKey *ecdsa.PrivateKey,
	fromAddress common.Address,
	targetNonce uint64,
	gasMultiplier float64,
) (*types.Transaction, error) {
	var (
		methodPrefix = "SpeedNonce"
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
