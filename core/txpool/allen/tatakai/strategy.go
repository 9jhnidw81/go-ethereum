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
	common2 "github.com/ethereum/go-ethereum/core/txpool/allen/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/crypto"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

// TODO: 动态递增gas (如果连续成功好几次，但是没有一次真正的成功，则考虑动态递增gas，有最高限制)
const (
	// 买入滑点
	slipPointBuy = 10
	// 卖出滑点
	slipPointSell = 90
	// gas滑点
	slipPointGas = 700
	// 授权gas滑点
	approveSlipPointGas = 200
	// gas价格滑点
	slipPointGasPrice = 700
	// 交易有效时间(仅用于合约，无法用于区块链网络有效时间)
	expireTime = time.Minute * 5
	// 默认gas(用于首次卖出代币，无法计算gas值的备选)
	defaultGas uint64 = 400000
	// 最大授权额度
	maxApproveAmount = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	// 前导交易量比例 60%
	frontRunRatio = 80
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
	Path      []common.Address
	AmountIn  *big.Int // 代币输入量
	AmountOut *big.Int // 代币输出量
	Deadline  *big.Int
}

func NewSandwichBuilder(ethClient *client.EthClient, parser *TransactionParser, pk *ecdsa.PrivateKey) *SandwichBuilder {
	return &SandwichBuilder{
		ethClient:   ethClient,
		parser:      parser,
		privateKey:  pk,
		FromAddress: crypto.PubkeyToAddress(pk.PublicKey),
	}
}

//单例模式测试
//直接发送2笔买入卖出的eth、yu交易，看看能不能成功，能成功再去夹其他人
//不管是提高手续费，还是啥反正一定要成功买入卖出再夹别人
//
//提高到超级高gas

func (b *SandwichBuilder) Build(ctx context.Context, tx *types.Transaction) ([]*types.Transaction, error) {
	// To地址判断
	if tx.To() == nil || *tx.To() != b.parser.routerAddress {
		return nil, common2.ErrNotUniswapTx
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
		return nil, common2.ErrNotUniswapBuyTx
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
	// 授权
	var (
		approveTx   *types.Transaction
		needApprove bool
	)
	if allowance.Cmp(big.NewInt(0)) == 0 && !b.isTokenApproved(path[1]) {
		amountIn := new(big.Int)
		amountIn.SetString(maxApproveAmount, 16)
		approveTx, err = b.approveTokens(ctx, path[1], amountIn, gasPrice, frontNonce)
		fmt.Println("授权", path[1], "err:", err)
		if err != nil {
			// 立即强制同步nonce
			syncErr := b.ethClient.ForceSyncNonce(ctx, b.FromAddress)
			log.Printf("[Fight] 交易失败，已触发nonce同步（结果:%v）", syncErr)
			return nil, err
		} else {
			b.setTokenApprove(path[1])
			needApprove = true
			frontNonce++ // TODO: 这里会有问题，假设授权没报错，但是三明治攻击失败，nonce已经递增了，此时会导致后面的nonce全部失败？
			backNonce++  // TODO: 可能需要定期检查nonce有效性？或者监控三明治攻击失败的时候，用自转0eth的方式重新清洗nonce？
		}
	}

	frontTx, frontInAmount, victimInAmount, err := b.buildFrontRunTx(ctx, tx, gasPrice, method, params, frontNonce)
	if err != nil {
		return nil, err
	}
	backTx, err := b.buildBackRunTx(ctx, frontTx, frontInAmount, victimInAmount, gasPrice, params["path"].([]common.Address), backNonce)
	if err != nil {
		return nil, err
	}

	pairAddress, err := b.GetPairAddress(path[0], path[1])
	if err != nil {
		return nil, err
	}

	// 新增利润空间判断
	isProfitable, err := b.isArbitrageProfitable(
		ctx,
		frontInAmount,                    // 第一笔交易输入量
		victimInAmount,                   // 受害者输入量
		frontTx.GasPrice(),               // Gas价格
		frontTx.Gas()+backTx.Gas()+21000, // 三笔交易总Gas（假设第三方交易gas）TODO： 未考虑授权的Gas
		&pairAddress,                     // 交易对地址
		path,                             // 交易对路径
	)
	if err != nil {
		return nil, err
	}
	if !isProfitable {
		//return nil, ErrNotEnoughProfit
		//return nil, errors.New("1")
	}

	if needApprove && approveTx != nil {
		return []*types.Transaction{approveTx, frontTx, tx, backTx}, nil
	}
	return []*types.Transaction{frontTx, backTx}, nil
	//return []*types.Transaction{frontTx, tx, backTx}, nil
}

// 构建买入交易（前跑）
func (b *SandwichBuilder) buildFrontRunTx(ctx context.Context, targetTx *types.Transaction, gasPrice *big.Int, method *abi.Method, params map[string]interface{}, frontNonce uint64) (*types.Transaction, *big.Int, *big.Int, error) {
	const (
		methodPrefix = "buildFrontRunTx"
	)

	// 解析目标交易参数
	swapParams, err := b.parseSwapParams(method, params)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("[%s] 解析目标交易参数失败: %w", methodPrefix, err)
	}

	// 获取交易对储备, [WETH,XX] or [Yu,BTC]
	path := params["path"].([]common.Address)

	// 获取输入资产类型，是否ETH
	inputIsETH := path[0] == b.parser.wethAddress

	// 获取当前交易对的储备量
	pairAddress, err := b.GetPairAddress(path[0], path[1])
	poolInToken, poolOutToken, err := b.getPoolReserves(ctx, &pairAddress, path[0], path[1])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("[%s] 获取资金池储备失败: %w", methodPrefix, err)
	}

	// 受害者输入量，默认取ETH的值
	victimInAmount, frontInAmount := targetTx.Value(), new(big.Int)
	if !inputIsETH {
		// 从交易参数获取代币输入量
		victimInAmount = swapParams.AmountIn
	}
	// 计算前导交易量（受害者交易量的60%）
	frontInAmount = new(big.Int).Mul(victimInAmount, big.NewInt(frontRunRatio))
	frontInAmount.Div(frontInAmount, big.NewInt(100))
	// 模拟输入交易扣除的手续费，这里是0.3%
	effectiveInput := new(big.Int).Mul(frontInAmount, big.NewInt(997))
	effectiveInput.Div(effectiveInput, big.NewInt(1000))

	// 预期通过输入得到的代币数量
	frontTokenOut := calculateOutputAmount(effectiveInput, poolInToken, poolOutToken)

	// 重新计算最小输出（基于前导量+负滑点）, 可以根据调用路由合约的 getAmountsOut 判断现在能接收到的价格是否合理
	minAmountOut := CalculateWithSlippageEx(frontTokenOut, -slipPointBuy)

	// 构造交易数据
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())
	var data []byte
	if inputIsETH {
		// ETH兑换
		data, err = b.parser.uniswapABI.Pack(config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens,
			minAmountOut,    // 愿意接受的 最少能换到多少代币，少于会失败
			swapParams.Path, // 兑换代币的路径 [weth, token]
			b.FromAddress,   // 接收代币的路径
			deadline,        // 过期时间
		)
	} else {
		// 代币兑换
		data, err = b.parser.uniswapABI.Pack(config.MethodSwapExactTokensForTokensSupportingFeeOnTransferTokens,
			frontInAmount,   // 花出去的 代币 A 的数量
			minAmountOut,    // 愿意接受的 最少能换到多少代币 B，少于会失败
			swapParams.Path, // 兑换代币的路径 [代币A, 代币B]
			b.FromAddress,   // 接收代币的地址
			deadline,        // 过期时间
		)
	}

	if err != nil {
		return nil, nil, nil, fmt.Errorf("[%s] 交易数据构造失败: %w", methodPrefix, err)
	}

	// 估算Gas Limit
	gasLimit := defaultGas
	callMsg := ethereum.CallMsg{
		From:     b.FromAddress,
		To:       targetTx.To(),
		Value:    big.NewInt(0), // 代币兑换代币，则为0
		GasPrice: gasPrice,
		Data:     data,
	}
	if inputIsETH {
		callMsg.Value = frontInAmount // ETH兑换，则为value
	}
	estimatedGas, err := b.ethClient.EstimateGas(ctx, callMsg)
	fmt.Println("[Fight] 前导交易预计需要耗费的gas", estimatedGas, err)
	// 处理gas估算错误
	if estimatedGas > 0 {
		gasLimit = estimatedGas
	}
	fmt.Println("[Fight] GasLimit", gasLimit, "GasLimit+滑点", CalculateUint64SlipPoint(gasLimit, slipPointGas))
	// 构建并签名交易
	txInner := &types.LegacyTx{
		Nonce:    frontNonce,
		GasPrice: gasPrice,
		Gas:      CalculateUint64SlipPoint(gasLimit, slipPointGas),
		To:       targetTx.To(),
		Value:    big.NewInt(0), // 代币兑换代币，则为0
		Data:     data,
	}
	if inputIsETH {
		txInner.Value = frontInAmount // ETH兑换，则为value
	}
	tx := types.NewTx(txInner)
	fmt.Printf("[Fight] Front tx nonce:%+v gasPrice:%+v gas:%+v to:%+v value:%+v\n", txInner.Nonce, txInner.GasPrice, txInner.Gas, txInner.To, txInner.Value)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(b.ethClient.Config.ChainID), b.privateKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("[%s] 交易签名失败: %w", methodPrefix, err)
	}

	return signedTx, frontInAmount, victimInAmount, nil
}

// 构建卖入交易（后跑）
func (b *SandwichBuilder) buildBackRunTx(ctx context.Context, frontTx *types.Transaction, frontInAmount, victimInAmount, gasPrice *big.Int, path []common.Address, backNonce uint64) (*types.Transaction, error) {
	const (
		methodPrefix = "buildBackRunTx"
	)
	// 获取当前资金池储备
	pairAddress, err := b.GetPairAddress(path[0], path[1])
	poolInToken, poolOutToken, err := b.getPoolReserves(ctx, &pairAddress, path[0], path[1])
	if err != nil {
		return nil, fmt.Errorf("[%s] 获取资金池储备失败: %w", methodPrefix, err)
	}

	// 获取输出资产类型，是否ETH
	outputIsETH := path[0] == b.parser.wethAddress

	// 模拟前导交易影响，即模拟前导输入得到输出
	frontEffective := new(big.Int).Mul(frontInAmount, big.NewInt(997))
	frontEffective.Div(frontEffective, big.NewInt(1000))
	frontTokenOut := calculateOutputAmount(frontEffective, poolInToken, poolOutToken)
	// 模拟前导交易之后的weth储备量
	reserveAfterFrontWETH := new(big.Int).Add(poolInToken, frontEffective)
	// 模拟前导交易之后的token储备量
	reserveAfterFrontToken := new(big.Int).Sub(poolOutToken, frontTokenOut)

	// 模拟受害者交易影响
	victimEffective := new(big.Int).Mul(victimInAmount, big.NewInt(997))
	victimEffective.Div(victimEffective, big.NewInt(1000))
	victimTokenOut := calculateOutputAmount(victimEffective, reserveAfterFrontWETH, reserveAfterFrontToken)

	// 受害者买入之后的储备量
	finalReserveWETH := new(big.Int).Add(reserveAfterFrontWETH, victimEffective)
	finalReserveToken := new(big.Int).Sub(reserveAfterFrontToken, victimTokenOut)

	// 计算后导交易输出
	backEffective := new(big.Int).Mul(frontTokenOut, big.NewInt(997))
	backEffective.Div(backEffective, big.NewInt(1000))
	expectedETH := calculateOutputAmount(backEffective, finalReserveToken, finalReserveWETH)

	// 动态滑点计算
	amountOutMin := CalculateWithSlippageEx(expectedETH, -slipPointSell)
	if amountOutMin.Cmp(common.Big0) <= 0 {
		return nil, fmt.Errorf("[%s] 无效滑点计算 预期ETH/InToken:%s", methodPrefix, expectedETH.String())
	}
	fmt.Printf("[Fight] 后导预期输入的token数量:%+v, 预期得到的eth数量:%+v, 减去滑点之后预期得到的eth数量:%+v\n", backEffective, expectedETH, amountOutMin)

	// 构造交易数据
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())
	reversePath := ReversePath(path)
	methodName := config.MethodSwapExactTokensForTokensSupportingFeeOnTransferTokens // 默认代币兑换代币
	if outputIsETH {
		methodName = config.MethodSwapExactTokensForETHSupportingFeeOnTransferTokens // 否则为卖出eth
	}
	data, err := b.parser.uniswapABI.Pack(methodName,
		frontTokenOut, // 花出去的 代币 A 的精确数量，使用前导获得的全部代币 // 花多少代币A去换 ETH
		big.NewInt(0), // 愿意接受的 最少能换到多少 ETH // 最少换到多少 ETH
		reversePath,   // 交易路径[代币A, 代币B] [代币地址, WETH地址]
		b.FromAddress, // ETH接收地址
		deadline,      // 交易过期时间戳
	)
	fmt.Println("卖出交易的打包参数", methodName, frontTokenOut, amountOutMin, reversePath, b.FromAddress, deadline)
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
	fmt.Printf("[Fight] 后导模拟计算建议gas请求计算gas的参数:%+v, err:%+v\n", ethereum.CallMsg{
		From:     b.FromAddress,
		To:       frontTx.To(),
		GasPrice: gasPrice,
		Data:     data,
	}, err)

	// 处理gas估算错误
	if estimatedGas > 0 {
		gasLimit = estimatedGas
	}

	// 构建并签名交易
	txInner := &types.LegacyTx{
		Nonce:    backNonce,
		GasPrice: gasPrice,
		Gas:      CalculateUint64SlipPoint(gasLimit, slipPointGas),
		To:       frontTx.To(),
		Data:     data,
	}
	tx := types.NewTx(txInner)
	fmt.Printf("[Fight] Back tx nonce:%+v gasPrice:%+v gas:%+v to:%+v value:%+v\n", txInner.Nonce, txInner.GasPrice, txInner.Gas, txInner.To, txInner.Value)

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
	case config.MethodSwapExactETHForTokens, config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens:
		// ETH 作为输入，amountOutMin 存在
		swapParams.AmountOut = params["amountOutMin"].(*big.Int)

	case config.MethodSwapETHForExactTokens:
		// 有固定ETH换最多代币
		swapParams.AmountOut = params["amountOut"].(*big.Int)

	case config.MethodSwapExactTokensForTokens:
		// 固定输入量，求最大输出
		swapParams.AmountIn = params["amountIn"].(*big.Int)
		swapParams.AmountOut = params["amountOutMin"].(*big.Int)

	case config.MethodSwapTokensForExactTokens:
		// 固定输出量，求最小输入
		swapParams.AmountIn = params["amountInMax"].(*big.Int)
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

	inputIsETH := tokenA == b.parser.wethAddress

	// 3. 调用工厂合约
	to := &b.parser.factoryAddress2
	if inputIsETH {
		to = &b.parser.factoryAddress
	}
	result, err := b.ethClient.CallContract(context.Background(), ethereum.CallMsg{
		To:   to,
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

//// 利润判断核心方法
//func (b *SandwichBuilder) isArbitrageProfitable(
//	ctx context.Context,
//	frontRunAmount *big.Int, // 第一笔交易输入量
//	victimAmount *big.Int, // 第二笔交易输入量
//	gasPrice *big.Int, // Gas价格
//	totalGas uint64, // 总Gas消耗
//	pairAddress *common.Address, // 交易对地址
//	inputAddress common.Address, // 输入代币地址
//) (bool, error) {
//	// 1. 获取当前资金池储备
//	// 获取输入资产类型，是否ETH
//	inputIsETH := inputAddress == b.parser.wethAddress
//	reserveWETH, reserveToken, err := b.getPoolReserves(ctx, pairAddress, inputIsETH)
//	if err != nil {
//		return false, err
//	}
//
//	// 2. 计算三笔交易后的资金池变化
//	// 第一笔交易（前跑）
//	effectiveIn1 := new(big.Int).Mul(frontRunAmount, big.NewInt(997))
//	effectiveIn1.Div(effectiveIn1, big.NewInt(1000))
//	tokenOut := calculateOutputAmount(effectiveIn1, reserveWETH, reserveToken)
//
//	reserveWETHAfter1 := new(big.Int).Add(reserveWETH, effectiveIn1)
//	reserveTokenAfter1 := new(big.Int).Sub(reserveToken, tokenOut)
//
//	// 第二笔交易（受害者交易）
//	effectiveIn2 := new(big.Int).Mul(victimAmount, big.NewInt(997))
//	effectiveIn2.Div(effectiveIn2, big.NewInt(1000))
//	tokenOut2 := calculateOutputAmount(effectiveIn2, reserveWETHAfter1, reserveTokenAfter1)
//
//	reserveWETHAfter2 := new(big.Int).Add(reserveWETHAfter1, effectiveIn2)
//	reserveTokenAfter2 := new(big.Int).Sub(reserveTokenAfter1, tokenOut2)
//
//	// 第三笔交易（后跑卖出）
//	effectiveTokenIn := new(big.Int).Mul(tokenOut, big.NewInt(997))
//	effectiveTokenIn.Div(effectiveTokenIn, big.NewInt(1000))
//	wethOut := calculateOutputAmount(effectiveTokenIn, reserveTokenAfter2, reserveWETHAfter2)
//
//	// 3. 计算总成本和利润
//	totalCost := new(big.Int).Set(frontRunAmount)
//
//	// 计算Gas成本（转换为WETH）
//	gasCost := new(big.Int).Mul(
//		big.NewInt(int64(totalGas)),
//		gasPrice,
//	)
//	totalCost.Add(totalCost, gasCost)
//
//	// 4. 最终利润判断
//	profit := new(big.Int).Sub(wethOut, totalCost)
//	fmt.Println("[Fight] 最终利润", WeiToEth(profit))
//	// 利润必须>0
//	return profit.Cmp(big.NewInt(0)) > 0, nil
//}

func (b *SandwichBuilder) isArbitrageProfitable(
	ctx context.Context,
	frontRunAmount *big.Int, // 前导交易输入量（可能是ETH或代币）
	victimAmount *big.Int, // 受害者交易输入量
	gasPrice *big.Int, // Gas价格（单位：Wei）
	totalGas uint64, // 总Gas消耗
	pairAddress *common.Address, // 直接交易对地址
	path []common.Address, // 完整交易路径
) (bool, error) {
	// 1. 输入资产类型判断
	inputIsETH := path[0] == b.parser.wethAddress

	// 2. 获取输入代币的ETH价值（如果是代币）
	var (
		costETH     *big.Int // 前导交易成本（ETH计价）
		outputIsETH bool     // 最终输出是否为ETH
		expectedETH *big.Int // 预期利润（ETH计价）
	)

	// 输入为ETH的情况
	if inputIsETH {
		costETH = new(big.Int).Set(frontRunAmount)
		outputIsETH = path[len(path)-1] == b.parser.wethAddress
	} else {
		// 获取输入代币/WETH交易对储备
		tokenWethPair, err := b.GetPairAddress(path[0], path[1])
		if err != nil {
			return false, fmt.Errorf("找不到WETH交易对: %v", err)
		}
		reserveToken, reserveWETH, err := b.getPoolReserves(ctx, &tokenWethPair, path[0], path[1])
		if err != nil {
			return false, err
		}

		// 计算输入代币的ETH价值
		effectiveIn := new(big.Int).Mul(frontRunAmount, big.NewInt(997))
		effectiveIn.Div(effectiveIn, big.NewInt(1000))
		costETH = calculateOutputAmount(effectiveIn, reserveToken, reserveWETH)

		// 判断最终输出类型
		outputIsETH = path[len(path)-1] == b.parser.wethAddress
	}

	// 3. 核心套利计算逻辑
	// 获取初始储备（自动处理ETH/代币顺序）
	reserveIn, reserveOut, err := b.getPoolReserves(ctx, pairAddress, path[0], path[1])
	if err != nil {
		return false, err
	}

	// 模拟前导交易影响
	effectiveFront := new(big.Int).Mul(frontRunAmount, big.NewInt(997))
	effectiveFront.Div(effectiveFront, big.NewInt(1000))
	frontOut := calculateOutputAmount(effectiveFront, reserveIn, reserveOut)

	reserveAfterFrontIn := new(big.Int).Add(reserveIn, effectiveFront)
	reserveAfterFrontOut := new(big.Int).Sub(reserveOut, frontOut)

	// 模拟受害者交易影响
	effectiveVictim := new(big.Int).Mul(victimAmount, big.NewInt(997))
	effectiveVictim.Div(effectiveVictim, big.NewInt(1000))
	victimOut := calculateOutputAmount(effectiveVictim, reserveAfterFrontIn, reserveAfterFrontOut)

	reserveAfterVictimIn := new(big.Int).Add(reserveAfterFrontIn, effectiveVictim)
	reserveAfterVictimOut := new(big.Int).Sub(reserveAfterFrontOut, victimOut)

	// 模拟后导交易卖出
	effectiveBack := new(big.Int).Mul(frontOut, big.NewInt(997))
	effectiveBack.Div(effectiveBack, big.NewInt(1000))
	backOut := calculateOutputAmount(effectiveBack, reserveAfterVictimOut, reserveAfterVictimIn)

	// 4. 处理多跳路径输出转换
	if !outputIsETH {
		// 获取最终输出代币的ETH价值
		outputPair, err := b.GetPairAddress(path[0], path[1])
		if err != nil {
			return false, fmt.Errorf("无法获取输出代币价格: %v", err)
		}
		reserveToken, reserveWETH, err := b.getPoolReserves(ctx, &outputPair, path[0], path[1])
		if err != nil {
			return false, err
		}

		effectiveOutput := new(big.Int).Mul(backOut, big.NewInt(997))
		effectiveOutput.Div(effectiveOutput, big.NewInt(1000))
		expectedETH = calculateOutputAmount(effectiveOutput, reserveToken, reserveWETH)
	} else {
		expectedETH = backOut
	}

	// 5. 计算总成本（ETH计价）
	gasCost := new(big.Int).Mul(
		big.NewInt(int64(totalGas)),
		gasPrice,
	)
	totalCost := new(big.Int).Add(costETH, gasCost)

	// 6. 最终利润判断
	profit := new(big.Int).Sub(expectedETH, totalCost)
	fmt.Printf("[Profit] 输入成本: %s | 实际利润：%s | 预期收入: %s | Gas成本: %s\n",
		WeiToEth(totalCost), WeiToEth(profit), WeiToEth(expectedETH), WeiToEth(gasCost))

	return profit.Cmp(big.NewInt(0)) > 0, nil
}

// 辅助方法：获取资金池储备
// 按照输入代币, 输出代币的顺序返回, 比如weth兑换link, 则返回weth,link
func (b *SandwichBuilder) getPoolReserves(ctx context.Context, pairAddress *common.Address, tokenA, tokenB common.Address) (*big.Int, *big.Int, error) {
	var reserves struct {
		Reserve0           *big.Int `abi:"_reserve0"`           // 地址较小的代币（token0）的储备量
		Reserve1           *big.Int `abi:"_reserve1"`           // 地址较大的代币（token1）的储备量
		BlockTimestampLast uint32   `abi:"_blockTimestampLast"` // 最后一次更新储备量的区块时间戳
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

	cmpRes := CompareAddress(tokenA, tokenB)
	switch {
	case cmpRes < 0:
		// A小于B
		return reserves.Reserve0, reserves.Reserve1, nil
	case cmpRes > 0:
		// A大于B
		return reserves.Reserve1, reserves.Reserve0, nil
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
func (b *SandwichBuilder) approveTokens(ctx context.Context, tokenAddr common.Address, amountIn, gasPrice *big.Int, nonce uint64) (*types.Transaction, error) {
	// 打包调用数据
	approveData, err := b.parser.erc20ABI.Pack("approve", b.parser.routerAddress, amountIn)
	if err != nil {
		return nil, err
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
		return nil, err
	}

	return signedTx, nil
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
