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
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// 买入滑点
	slipPointBuy = 5
	// 卖出滑点
	slipPointSell = 2
	// gas滑点
	slipPointGas = 300
	// gas价格滑点
	slipPointGasPrice = 200
	// 交易有效时间(仅用于合约，无法用于区块链网络有效时间)
	expireTime = time.Minute * 5
	// 默认gas(用于首次卖出代币，无法计算gas值的备选)
	defaultGas uint64 = 150000
	// Uniswap V2 Pair初始化代码哈希
	initCodeHash = "0x96e8ac4277198ff8b6f785478aa9a39f403cb768dd02cbee326c3e7da348845f"
)

type SandwichBuilder struct {
	ethClient   *client.EthClient
	parser      *TransactionParser
	privateKey  *ecdsa.PrivateKey
	fromAddress common.Address
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
		fromAddress: crypto.PubkeyToAddress(pk.PublicKey),
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
	if !ok || err != nil {
		return nil, err
	}

	// 同步链上nonce
	err = b.ethClient.SyncNonce(ctx, b.fromAddress)
	if err != nil {
		return nil, err
	}
	// 获取连续nonce
	frontNonce, err := b.ethClient.GetSequentialNonce(ctx, b.fromAddress)
	if err != nil {
		return nil, err
	}
	backNonce := frontNonce + 1

	//接下来的问题，批量买入卖出交易，需要成功率很高
	//	其次才是测试网的测试

	// 实现完整的交易构建逻辑
	gasPrice, err := b.ethClient.GetDynamicGasPrice(ctx)
	if err != nil {
		return nil, err
	}

	// 滑点价格
	gasPrice = CalculateWithSlippageEx(gasPrice, slipPointGasPrice)

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

	backTx, err := b.buildBackRunTx(ctx, frontTx, gasPrice, method, params, backNonce)
	if err != nil {
		return nil, err
	}
	// 普通发送
	//err = b.ethClient.SendTransaction(context.Background(), backTx)
	//if err != nil {
	//	return nil, err
	//}
	//fmt.Println("发送卖出交易成功")

	path := params["path"].([]common.Address)
	pairAddress, err := b.GetPairAddress(path[0], path[1])
	if err != nil {
		return nil, err
	}

	// 从原始交易参数中获取受害者实际输入量
	victimInput, err := b.getVictimInputAmount(method, params)
	if err != nil {
		return nil, err
	}

	// 新增利润空间判断
	isProfitable, err := b.isArbitrageProfitable(
		ctx,
		frontTx.Value(),                  // 第一笔交易输入量
		victimInput,                      // 正确获取受害者输入量
		frontTx.GasPrice(),               // Gas价格
		frontTx.Gas()+backTx.Gas()+21000, // 三笔交易总Gas（假设第三方交易gas）
		&pairAddress,                     // 交易对地址
	)
	if err != nil {
		return nil, err
	}
	if !isProfitable {
		return nil, errors.New("无利润空间")
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

	// 计算滑点后的输入金额（5%滑点）
	frontRunAmount := CalculateWithSlippageEx(targetTx.Value(), slipPointBuy)

	// 构造交易数据
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())

	data, err := b.parser.uniswapABI.Pack("swapExactETHForTokensSupportingFeeOnTransferTokens",
		swapParams.AmountOutMin, // 使用原始交易的输出限制
		swapParams.Path,
		b.fromAddress,
		deadline,
	)
	if err != nil {
		return nil, fmt.Errorf("[%s] 交易数据构造失败: %w", methodPrefix, err)
	}

	// 估算Gas Limit
	gasLimit := defaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, ethereum.CallMsg{
		From:     b.fromAddress,
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

func (b *SandwichBuilder) buildBackRunTx(ctx context.Context, frontTx *types.Transaction, gasPrice *big.Int, method *abi.Method, params map[string]interface{}, backNonce uint64) (*types.Transaction, error) {
	const (
		methodPrefix = "buildBackRunTx"
	)
	// 解析前导交易参数
	frontParams, err := b.parseSwapParams(method, params)
	if err != nil {
		return nil, fmt.Errorf("[%s] 前导交易参数解析失败: %w", methodPrefix, err)
	}

	// 计算滑点后的输出（2%滑点）
	backRunAmountOut := CalculateWithSlippageEx(frontParams.AmountOutMin, slipPointSell)

	// 临时设置为0，不检测滑点
	backRunAmountOut = big.NewInt(0)

	// 反转交易路径
	reversePath := ReversePath(frontParams.Path)

	// 构造交易数据
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())

	fmt.Println(frontParams.AmountOutMin,
		backRunAmountOut,
		reversePath,
		b.fromAddress,
		deadline)
	data, err := b.parser.uniswapABI.Pack("swapExactTokensForETH",
		frontParams.AmountOutMin,
		backRunAmountOut,
		reversePath,
		b.fromAddress,
		deadline,
	)
	if err != nil {
		return nil, fmt.Errorf("交易数据构造失败: %w", err)
	}

	// 修改后的gas估算逻辑
	gasLimit := defaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, ethereum.CallMsg{
		From:     b.fromAddress,
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
		return nil, fmt.Errorf("unsupported method: %s", method.Name)
	}

	return swapParams, nil
}

// GetPairAddress 获取交易对地址
// 链接参考:https://docs.uniswap.org/contracts/v2/guides/smart-contract-integration/getting-pair-addresses
func (b *SandwichBuilder) GetPairAddress(tokenA, tokenB common.Address) (common.Address, error) {
	// 1. 正确排序代币地址
	token0, token1 := SortTokens(tokenA, tokenB)

	// 2. 计算salt（keccak256(abi.encodePacked(token0, token1))）
	salt := crypto.Keccak256Hash(append(token0.Bytes(), token1.Bytes()...))

	// 3. 处理initCodeHash（去掉0x前缀后转为bytes）
	initHash := common.FromHex(initCodeHash)

	// 4. 正确的Create2地址计算
	pairAddress := crypto.CreateAddress2(
		b.parser.factoryAddress,
		salt,
		initHash,
	)

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
	fmt.Println("最终利润", WeiToEth(profit))
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

func (b *SandwichBuilder) getVictimInputAmount(method *abi.Method, params map[string]interface{}) (*big.Int, error) {
	switch method.Name {
	case config.MethodSwapExactETHForTokens:
		return params["amountOutMin"].(*big.Int), nil
	case config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens:
		return params["amountOutMin"].(*big.Int), nil
	default:
		return nil, fmt.Errorf("unsupported method: %s", method.Name)
	}
}

// 辅助方法：Uniswap输出量计算
func calculateOutputAmount(inputAmount, inputReserve, outputReserve *big.Int) *big.Int {
	numerator := new(big.Int).Mul(inputAmount, outputReserve)
	denominator := new(big.Int).Add(inputReserve, inputAmount)
	return new(big.Int).Div(numerator, denominator)
}
