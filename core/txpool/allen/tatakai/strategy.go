package tatakai

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/client"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/crypto"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// 买入滑点
	slipPointBuy = 5
	// 卖出滑点
	slipPointSell = 2
	// gas滑点
	slipPointGas = 700
	// 授权gas滑点
	approveSlipPointGas = 200
	// gas价格滑点
	slipPointGasPrice = 700
	// 交易有效时间(仅用于合约，无法用于区块链网络有效时间)
	expireTime = time.Minute * 5
	// 默认gas(用于首次卖出代币，无法计算gas值的备选)
	defaultGas uint64 = 150000
	// 最大授权额度
	maxApproveAmount = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	// 前导交易量比例 60%
	frontRunRatio = 60
	// 后导交易量比例 60%
	backRunRatio = 60
)

type SandwichBuilder struct {
	ethClient   *client.EthClient
	parser      *TransactionParser
	privateKey  *ecdsa.PrivateKey
	FromAddress common.Address

	// 代币授权的状态，避免重复授权
	approveTokenMap sync.Map
}

type SwapParams struct {
	Path         []common.Address
	AmountIn     *big.Int // 用于 swapExactTokensFor...
	AmountInMax  *big.Int // 用于 swapTokensForExact...
	AmountOutMin *big.Int // 用于 swapETHForTokens
	AmountOut    *big.Int // 用于 swapForExactTokens
	Deadline     *big.Int
}

func NewSandwichBuilder(ethClient *client.EthClient, parser *TransactionParser, pk *ecdsa.PrivateKey) *SandwichBuilder {
	return &SandwichBuilder{
		ethClient:   ethClient,
		parser:      parser,
		privateKey:  pk,
		FromAddress: crypto.PubkeyToAddress(pk.PublicKey),
	}
}

func (b *SandwichBuilder) Build(ctx context.Context, tx *types.Transaction) ([]*types.Transaction, error) {
	// To地址判断
	if tx.To() == nil || *tx.To() != b.parser.routerAddress {
		return nil, ErrNotUniswapTx
	}
	// 解析方法跟参数
	method, params, err := b.parser.ParseMethodAndParams(tx)
	if err != nil {
		return nil, err
	}
	// 是否Uniswap买入交易
	ok, err := b.parser.IsBuyTransaction(params)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotUniswapBuyTx
	}

	// 同步链上nonce
	err = b.ethClient.SyncNonce(ctx, b.FromAddress)
	if err != nil {
		return nil, err
	}
	// 获取连续nonce
	frontNonce, err := b.ethClient.GetSequentialNonce(ctx, b.FromAddress)
	if err != nil {
		return nil, err
	}
	backNonce := frontNonce + 1

	// 实现完整的交易构建逻辑
	gasPrice, err := b.ethClient.GetDynamicGasPrice(ctx)
	if err != nil {
		return nil, err
	}

	// 滑点价格
	gasPrice = CalculateWithSlippageEx(gasPrice, slipPointGasPrice)

	// 直接授权（真实情况需要后授权，这里主要为了检验成功率
	// 判断授权额度
	path := params["path"].([]common.Address)
	allowance, err := b.getAllowance(ctx, path[1], b.parser.routerAddress)
	if err != nil {
		return nil, err
	}
	// 授权 TODO： 多个tx同一个token，导致多次授权->前者的txs的nonce失效
	if allowance.Cmp(big.NewInt(0)) == 0 && !b.isTokenApproved(path[1]) {
		amountIn := new(big.Int)
		amountIn.SetString(maxApproveAmount, 16)
		err = b.approveTokens(ctx, path[1], amountIn, gasPrice, frontNonce)
		fmt.Println("授权", path[1], "err:", err)
		if err != nil {
			// 立即强制同步nonce
			syncErr := b.ethClient.ForceSyncNonce(ctx, b.FromAddress)
			log.Printf("[Fight] 交易失败，已触发nonce同步（结果:%v）", syncErr)
			return nil, err
		} else {
			b.setTokenApprove(path[1])
			frontNonce++ // TODO: 这里会有问题，假设授权没报错，但是三明治攻击失败，nonce已经递增了，此时会导致后面的nonce全部失败？
			backNonce++  // TODO: 可能需要定期检查nonce有效性？或者监控三明治攻击失败的时候，用自转0eth的方式重新清洗nonce？
		}
	}

	frontTx, err := b.buildFrontRunTx(ctx, tx, gasPrice, method, params, frontNonce)
	if err != nil {
		return nil, err
	}
	// 普通发送
	//err = b.ethClient.SendTransaction(context.Background(), frontTx)
	//if err != nil {
	//	return nil, err
	//}
	//fmt.Println("发送买入交易成功")

	// 另外起一个协程，额度授权

	backTx, err := b.buildBackRunTx(ctx, frontTx, tx, gasPrice, method, params, backNonce)
	if err != nil {
		return nil, err
	}
	// 普通发送
	//err = b.ethClient.SendTransaction(context.Background(), backTx)
	//if err != nil {
	//	return nil, err
	//}
	//fmt.Println("发送卖出交易成功")
	pairAddress, err := b.GetPairAddress(path[0], path[1])
	if err != nil {
		return nil, err
	}

	// 从原始交易参数中获取受害者实际输入量
	victimInput, err := b.getVictimInputAmount(method, tx)
	if err != nil {
		return nil, err
	}

	// 新增利润空间判断
	isProfitable, err := b.isArbitrageProfitable(
		ctx,
		frontTx.Value(),                  // 第一笔交易输入量
		victimInput,                      // 正确获取受害者输入量
		frontTx.GasPrice(),               // Gas价格
		frontTx.Gas()+backTx.Gas()+21000, // 三笔交易总Gas（假设第三方交易gas）TODO： 未考虑授权的Gas
		&pairAddress,                     // 交易对地址
	)
	if err != nil {
		return nil, err
	}
	if !isProfitable {
		//return nil, ErrNotEnoughProfit
	}

	return []*types.Transaction{frontTx, tx, backTx}, nil
}

// 构建买入交易（前跑）
func (b *SandwichBuilder) buildFrontRunTx(ctx context.Context, targetTx *types.Transaction, gasPrice *big.Int, method *abi.Method, params map[string]interface{}, frontNonce uint64) (*types.Transaction, error) {
	const (
		methodPrefix = "buildFrontRunTx"
	)

	// 解析目标交易参数
	swapParams, err := b.parseSwapParams(method, params)
	if err != nil {
		return nil, fmt.Errorf("[%s] 解析目标交易参数失败: %w", methodPrefix, err)
	}

	// 获取交易对储备
	path := params["path"].([]common.Address)
	pairAddress, err := b.GetPairAddress(path[0], path[1])
	originalReserveWETH, originalReserveToken, err := b.getPoolReserves(ctx, &pairAddress)
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取资金池储备失败: %w", methodPrefix, err)
	}

	// 计算前导交易量（受害者交易量的60%）
	victimAmount := targetTx.Value()
	frontRunAmount := new(big.Int).Mul(victimAmount, big.NewInt(frontRunRatio))
	frontRunAmount.Div(frontRunAmount, big.NewInt(100))

	// 模拟前导交易影响
	frontEffective := new(big.Int).Mul(frontRunAmount, big.NewInt(997))
	frontEffective.Div(frontEffective, big.NewInt(1000))
	frontTokenOut := calculateOutputAmount(frontEffective, originalReserveWETH, originalReserveToken)

	// 重新计算最小输出（基于前导量）
	minAmountOut := CalculateWithSlippageEx(frontTokenOut, slipPointBuy)

	// 构造交易数据
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())
	data, err := b.parser.uniswapABI.Pack("swapExactETHForTokensSupportingFeeOnTransferTokens",
		minAmountOut,
		swapParams.Path,
		b.FromAddress,
		deadline,
	)
	if err != nil {
		return nil, fmt.Errorf("[%s] 交易数据构造失败: %w", methodPrefix, err)
	}

	// 估算Gas Limit
	gasLimit := defaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, ethereum.CallMsg{
		From:     b.FromAddress,
		To:       targetTx.To(),
		Value:    frontRunAmount,
		GasPrice: gasPrice,
		Data:     data,
	})
	// 处理gas估算错误
	if err == nil {
		gasLimit = estimatedGas
	}

	// 构建并签名交易
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    frontNonce,
		GasPrice: gasPrice,
		Gas:      CalculateUint64SlipPoint(gasLimit, slipPointGas),
		To:       targetTx.To(),
		Value:    frontRunAmount,
		Data:     data,
	})
	fmt.Printf("Front tx:%+v\n", tx)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(b.ethClient.Config.ChainID), b.privateKey)
	if err != nil {
		return nil, fmt.Errorf("[%s] 交易签名失败: %w", methodPrefix, err)
	}

	return signedTx, nil
}

// 构建卖入交易（后跑）
func (b *SandwichBuilder) buildBackRunTx(ctx context.Context, frontTx, targetTx *types.Transaction, gasPrice *big.Int, method *abi.Method, params map[string]interface{}, backNonce uint64) (*types.Transaction, error) {
	const (
		methodPrefix = "buildBackRunTx"
	)
	// 获取当前资金池储备
	path := params["path"].([]common.Address)
	pairAddress, err := b.GetPairAddress(path[0], path[1])
	originalReserveWETH, originalReserveToken, err := b.getPoolReserves(ctx, &pairAddress)
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取资金池储备失败: %w", methodPrefix, err)
	}

	// 获取前导交易实际量（已调整60%）
	frontRunAmount := frontTx.Value()

	// 模拟前导交易影响
	frontEffective := new(big.Int).Mul(frontRunAmount, big.NewInt(997))
	frontEffective.Div(frontEffective, big.NewInt(1000))
	frontTokenOut := calculateOutputAmount(frontEffective, originalReserveWETH, originalReserveToken)
	reserveAfterFrontWETH := new(big.Int).Sub(originalReserveWETH, frontEffective)
	reserveAfterFrontToken := new(big.Int).Add(originalReserveToken, frontTokenOut)

	// 模拟受害者交易影响
	var victimInput *big.Int
	switch method.Name {
	case config.MethodSwapExactETHForTokens, config.MethodSwapETHForExactTokens, config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens, config.MethodSwapExactTokensForTokens:
		victimInput = targetTx.Value() // 原始交易参数中的输入量
	default:
		return nil, fmt.Errorf("[%s] 模拟受害者交易影响 unsupported method: %s", methodPrefix, method.Name)
	}

	victimEffective := new(big.Int).Mul(victimInput, big.NewInt(997))
	victimEffective.Div(victimEffective, big.NewInt(1000))
	victimTokenOut := calculateOutputAmount(victimEffective, reserveAfterFrontWETH, reserveAfterFrontToken)

	// 最终储备状态
	finalReserveWETH := new(big.Int).Add(reserveAfterFrontWETH, victimEffective)
	finalReserveToken := new(big.Int).Sub(reserveAfterFrontToken, victimTokenOut)

	// 计算后导交易输出
	backEffective := new(big.Int).Mul(frontTokenOut, big.NewInt(997))
	backEffective.Div(backEffective, big.NewInt(1000))
	expectedETH := calculateOutputAmount(backEffective, finalReserveToken, finalReserveWETH)

	// 动态滑点计算
	amountOutMin := CalculateWithSlippageEx(expectedETH, slipPointSell)
	if amountOutMin.Cmp(common.Big0) <= 0 {
		return nil, fmt.Errorf("[%s] 无效滑点计算 预期ETH:%s", methodPrefix, expectedETH.String())
	}

	// 构造交易数据
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())
	reversePath := ReversePath(path)
	data, err := b.parser.uniswapABI.Pack("swapExactTokensForETHSupportingFeeOnTransferTokens",
		//data, err := b.parser.uniswapABI.Pack("swapExactTokensForETH", // 改用一种成功率较高的方式
		frontTokenOut, // 卖出的代币的精确数量，使用前导获得的全部代币
		amountOutMin,  // 动态计算卖出代币时的最小可接受ETH数量
		reversePath,   // 交易路径[代币地址, WETH地址]
		b.FromAddress, // ETH接收地址
		deadline,      // 交易过期时间戳
	)
	fmt.Println(frontTokenOut, amountOutMin, reversePath, b.FromAddress, deadline)
	if err != nil {
		return nil, fmt.Errorf("交易数据构造失败: %w", err)
	}

	// 修改后的gas估算逻辑
	gasLimit := defaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, ethereum.CallMsg{
		From:     b.FromAddress,
		To:       frontTx.To(),
		GasPrice: gasPrice,
		Data:     data,
	})

	// 处理gas估算错误
	if err == nil {
		gasLimit = estimatedGas
	}

	// 构建并签名交易
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    backNonce,
		GasPrice: gasPrice,
		Gas:      CalculateUint64SlipPoint(gasLimit, slipPointGas),
		To:       frontTx.To(),
		Data:     data,
	})
	fmt.Printf("Back tx:%+v\n", tx)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(b.ethClient.Config.ChainID), b.privateKey)
	if err != nil {
		return nil, fmt.Errorf("交易签名失败: %w", err)
	}

	return signedTx, nil
}

// 辅助方法：解析兑换参数
func (b *SandwichBuilder) parseSwapParams(method *abi.Method, params map[string]interface{}) (*SwapParams, error) {
	swapParams := &SwapParams{
		Path:     params["path"].([]common.Address),
		Deadline: params["deadline"].(*big.Int),
	}

	// 根据方法名区分参数
	switch method.Name {
	case config.MethodSwapExactETHForTokens, config.MethodSwapETHForExactTokens, config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens:
		// ETH 作为输入，amountOutMin 存在
		swapParams.AmountOutMin = params["amountOutMin"].(*big.Int)
		swapParams.AmountIn = nil // 从 tx.Value 中获取实际 ETH 数量

	case config.MethodSwapExactTokensForTokens:
		// 代币作为输入，amountIn 存在
		swapParams.AmountIn = params["amountIn"].(*big.Int)
		swapParams.AmountOutMin = params["amountOutMin"].(*big.Int)

	case config.MethodSwapTokensForExactTokens:
		// 代币作为输入，amountInMax 存在
		swapParams.AmountInMax = params["amountInMax"].(*big.Int)
		swapParams.AmountOut = params["amountOut"].(*big.Int)

	default:
		return nil, fmt.Errorf("解析参数 unsupported method: %s", method.Name)
	}

	return swapParams, nil
}

// GetPairAddress 获取交易对地址
// 链接参考:https://docs.uniswap.org/contracts/v2/guides/smart-contract-integration/getting-pair-addresses
func (b *SandwichBuilder) GetPairAddress(tokenA, tokenB common.Address) (common.Address, error) {
	// 1. 正确排序代币地址
	token0, token1 := SortTokens(tokenA, tokenB)

	// 2. 准备调用数据 (getPair(address,address))
	data, err := b.parser.factoryABI.Pack("getPair", token0, token1)
	if err != nil {
		return common.Address{}, fmt.Errorf("ABI打包失败: %w", err)
	}

	// 3. 调用工厂合约
	result, err := b.ethClient.CallContract(context.Background(), ethereum.CallMsg{
		To:   &b.parser.factoryAddress,
		Data: data,
	}, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("工厂合约调用失败: %w", err)
	}

	// 4. 解析返回地址
	if len(result) != 32 {
		return common.Address{}, fmt.Errorf("无效的返回数据长度: %d", len(result))
	}

	// 5. 转换为地址类型（最后20字节）
	addressBytes := result[12:32] // 截取后20字节
	pairAddress := common.BytesToAddress(addressBytes)

	// 6. 验证地址有效性
	if pairAddress == (common.Address{}) {
		return common.Address{}, errors.New("交易对不存在")
	}

	return pairAddress, nil
}

// 利润判断核心方法
func (b *SandwichBuilder) isArbitrageProfitable(
	ctx context.Context,
	frontRunAmount *big.Int, // 第一笔交易输入量
	victimAmount *big.Int, // 第二笔交易输入量
	gasPrice *big.Int, // Gas价格
	totalGas uint64, // 总Gas消耗
	pairAddress *common.Address, // 交易对地址
) (bool, error) {
	// 1. 获取当前资金池储备
	reserveWETH, reserveToken, err := b.getPoolReserves(ctx, pairAddress)
	if err != nil {
		return false, err
	}

	// 2. 计算三笔交易后的资金池变化
	// 第一笔交易（前跑）
	effectiveIn1 := new(big.Int).Mul(frontRunAmount, big.NewInt(997))
	effectiveIn1.Div(effectiveIn1, big.NewInt(1000))
	tokenOut := calculateOutputAmount(effectiveIn1, reserveWETH, reserveToken)

	reserveWETHAfter1 := new(big.Int).Add(reserveWETH, effectiveIn1)
	reserveTokenAfter1 := new(big.Int).Sub(reserveToken, tokenOut)

	// 第二笔交易（受害者交易）
	effectiveIn2 := new(big.Int).Mul(victimAmount, big.NewInt(997))
	effectiveIn2.Div(effectiveIn2, big.NewInt(1000))
	tokenOut2 := calculateOutputAmount(effectiveIn2, reserveWETHAfter1, reserveTokenAfter1)

	reserveWETHAfter2 := new(big.Int).Add(reserveWETHAfter1, effectiveIn2)
	reserveTokenAfter2 := new(big.Int).Sub(reserveTokenAfter1, tokenOut2)

	// 第三笔交易（后跑卖出）
	effectiveTokenIn := new(big.Int).Mul(tokenOut, big.NewInt(997))
	effectiveTokenIn.Div(effectiveTokenIn, big.NewInt(1000))
	wethOut := calculateOutputAmount(effectiveTokenIn, reserveTokenAfter2, reserveWETHAfter2)

	// 3. 计算总成本和利润
	totalCost := new(big.Int).Set(frontRunAmount)

	// 计算Gas成本（转换为WETH）
	gasCost := new(big.Int).Mul(
		big.NewInt(int64(totalGas)),
		gasPrice,
	)
	totalCost.Add(totalCost, gasCost)

	// 4. 最终利润判断
	profit := new(big.Int).Sub(wethOut, totalCost)
	fmt.Println("[Fight] 最终利润", WeiToEth(profit))
	// 利润必须>0
	return profit.Cmp(big.NewInt(0)) > 0, nil
}

// 辅助方法：获取资金池储备
func (b *SandwichBuilder) getPoolReserves(ctx context.Context, pairAddress *common.Address) (*big.Int, *big.Int, error) {
	var reserves struct {
		Reserve0           *big.Int `abi:"_reserve0"`
		Reserve1           *big.Int `abi:"_reserve1"`
		BlockTimestampLast uint32   `abi:"_blockTimestampLast"`
	}

	// 调用合约方法
	data, _ := b.parser.pairABI.Pack("getReserves")
	result, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{
		To:   pairAddress,
		Data: data,
	}, nil)
	if err != nil {
		return nil, nil, err
	}

	// 解析结果
	if err := b.parser.pairABI.UnpackIntoInterface(&reserves, "getReserves", result); err != nil {
		return nil, nil, err
	}

	// 验证代币顺序
	token0Addr, _ := b.getTokenAddress(ctx, pairAddress, "token0")

	if token0Addr == b.parser.wethAddress {
		return reserves.Reserve0, reserves.Reserve1, nil
	}
	return reserves.Reserve1, reserves.Reserve0, nil
}

// 新增：获取代币地址
func (b *SandwichBuilder) getTokenAddress(ctx context.Context, pairAddress *common.Address, method string) (common.Address, error) {
	data, _ := b.parser.pairABI.Pack(method)
	result, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{
		To:   pairAddress,
		Data: data,
	}, nil)
	if err != nil {
		return common.Address{}, err
	}

	var addr common.Address
	if err := b.parser.pairABI.UnpackIntoInterface(&addr, method, result); err != nil {
		return common.Address{}, err
	}
	return addr, nil
}

// 新增方法：查询代币授权额度
func (b *SandwichBuilder) getAllowance(ctx context.Context, tokenAddr, spender common.Address) (*big.Int, error) {
	// 构造调用数据
	data, err := b.parser.erc20ABI.Pack("allowance", b.FromAddress, spender)
	if err != nil {
		return nil, fmt.Errorf("打包调用数据失败: %w", err)
	}

	// 调用合约
	result, err := b.ethClient.CallContract(ctx, ethereum.CallMsg{
		To:   &tokenAddr,
		Data: data,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("合约调用失败: %w", err)
	}

	// 解析结果
	var allowance *big.Int
	if err := b.parser.erc20ABI.UnpackIntoInterface(&allowance, "allowance", result); err != nil {
		return nil, fmt.Errorf("解析结果失败: %w", err)
	}

	return allowance, nil
}

// 授权
func (b *SandwichBuilder) approveTokens(ctx context.Context, tokenAddr common.Address, amountIn, gasPrice *big.Int, nonce uint64) error {
	// 打包调用数据
	approveData, err := b.parser.erc20ABI.Pack("approve", b.parser.routerAddress, amountIn)
	if err != nil {
		return err
	}

	approveCallMsg := ethereum.CallMsg{
		From: b.FromAddress,
		To:   &tokenAddr,
		Data: approveData,
	}

	// 估算Gas Limit
	gasLimit := defaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, approveCallMsg)
	// 处理gas估算错误
	if err == nil {
		gasLimit = estimatedGas
	}

	// 创建交易
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &tokenAddr,
		Gas:      CalculateUint64SlipPoint(gasLimit, approveSlipPointGas),
		GasPrice: gasPrice,
		Data:     approveData,
	})

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(b.ethClient.Config.ChainID), b.privateKey)
	if err != nil {
		return err
	}

	// 发送交易
	err = b.ethClient.SendTransaction(context.Background(), signedTx)
	if err != nil {
		return err
	}

	fmt.Println("Tokens approved for spending.")
	return nil
}

// 获取受害者原始交易的实际输入量
func (b *SandwichBuilder) getVictimInputAmount(method *abi.Method, tx *types.Transaction) (*big.Int, error) {
	switch method.Name {
	case config.MethodSwapExactETHForTokens, config.MethodSwapETHForExactTokens, config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens, config.MethodSwapExactTokensForTokens:
		return tx.Value(), nil
	default:
		return nil, fmt.Errorf("获取受害者原始交易输入量 unsupported method: %s", method.Name)
	}
}

// 辅助方法：判断代币是否授权
func (b *SandwichBuilder) isTokenApproved(token common.Address) bool {
	_, ok := b.approveTokenMap.Load(token)
	return ok
}

// 辅助方法：代币授权标记
func (b *SandwichBuilder) setTokenApprove(token common.Address) {
	b.approveTokenMap.Store(token, true)
}

// 辅助方法：Uniswap输出量计算
func calculateOutputAmount(inputAmount, inputReserve, outputReserve *big.Int) *big.Int {
	numerator := new(big.Int).Mul(inputAmount, outputReserve)
	denominator := new(big.Int).Add(inputReserve, inputAmount)
	return new(big.Int).Div(numerator, denominator)
}
