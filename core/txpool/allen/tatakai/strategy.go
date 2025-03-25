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
	"golang.org/x/sync/errgroup"
	"log"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

// 优化：
// 1.单位时间为（同一区块内），同一个代币的交易可以多次夹击，但可能成功率会比较低（备选、单次夹击（正常gas）+多次夹击（更高gas）一起）

// TODO: 动态递增gas (如果连续成功好几次，但是没有一次真正的成功，则考虑动态递增gas，有最高限制)
// TODO: 测试网络先保证一直夹，再确保主网也能一直夹
// TODO：主网不判断利润直接夹？
// TODO: 最低最高gas并行发送
// TODO: 一个受害者tx，并行发起多个交易，其中看似不同的bundle，实则攻击同一个代币（比如可插入黑洞转入的gas？），矿工拿到交易的时候虽然交易没啥意义，但是矿工能拿到额外一笔gas费用
const (
	// 买入滑点
	slipPointBuy = 10
	// 卖出滑点
	slipPointSell = 90
	// Gas limit滑点，最高使用的gas上限，若上限1000，实际使用100，则会返还900，所以这里往大了设置没关系
	slipPointGasLimit = 500
	// 授权gas滑点
	approveSlipPointGas = 200
	// 交易有效时间(仅用于合约，无法用于区块链网络有效时间)
	expireTime = time.Minute * 5
	// 最大授权额度
	maxApproveAmount = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	// 前导交易量比例 60%
	frontRunRatio = 80
	// 后导交易量比例 60%
	backRunRatio = 60
	// 每次实际交易的千分比 千分位, 997=>3手续费，即0.3%手续费
	actualTradeRatio = 997
	// 计算利润空间的前导交易滑点
	slipPointFrontGasLimit = 30
	// 计算利润空间的后导交易滑点
	slipPointBackGasLimit = 30
	// ctx超时时间
	ctxExpireTime = time.Second * 20
	// flashbot重试次数
	flashbotRetryCount = 3
)

var (
	// 每次滑点增长幅度
	slipPointIncreaseRate int32 = 100
	// 最大滑点
	slipPointIncreaseMax int32 = 5000
	// gas价格滑点，100为递增1倍，125为1.25倍，最低倍数（gasPrice与gasTipCap一致）
	slipPointGasPriceMin int32 = 100
	// 矿工小费gas滑点，100为递增1倍，125为1.25倍，最低倍数
	slipPointGasTipCapMin int32 = 100
	// gas价格滑点，100为递增1倍，125为1.25倍，最高倍数（gasPrice与gasTipCap一致）
	slipPointGasPriceMax int32 = 100 * 20
	// 矿工小费gas滑点，100为递增1倍，125为1.25倍，最高倍数
	slipPointGasTipCapMax int32 = 100 * 20
	// 每次递增倍数，在原来的基础上递增多少倍
	slipPointIncreasePer int32 = 200
)

type SandwichBuilder struct {
	ethClient   *client.EthClient
	fbClient    *client.FlashbotClient
	parser      *TransactionParser
	privateKey  *ecdsa.PrivateKey
	FromAddress common.Address
	DefaultGas  uint64

	// 代币授权的状态，避免重复授权
	approveTokenMap sync.Map
}

type SwapParams struct {
	Path      []common.Address
	AmountIn  *big.Int // 代币输入量
	AmountOut *big.Int // 代币输出量
	Deadline  *big.Int // 交易有效时间
}

type BuildBundleTxParams struct {
	// 受害者原始交易
	VictimTx *types.Transaction
	//受害者交易输入数量(ETH or Token)
	VictimInAmount *big.Int
	// 前导交易输入数量(ETH or Token)
	FrontInAmount *big.Int
	// 矿工小费Gas
	GasTipCap *big.Int
	// Gas 价格
	GasPrice *big.Int
	// 授权交易nonce
	ApproveNonce uint64
	// 前导交易nonce
	FrontNonce uint64
	// 后导交易nonce
	BackNonce uint64
	// 交易对地址
	PairAddress common.Address
	// 交易对地址数组
	Path []common.Address
	// 输入代币储备量
	ReserveInput *big.Int
	// 输出代币储备量
	ReserveOutput *big.Int
	// 需要授权
	NeedApprove bool
}

type BuildFrontRunTxParams struct {
	// 受害者原始交易
	VictimTx *types.Transaction
	// 前导交易输入数量(ETH or Token)
	FrontInAmount *big.Int
	// 矿工小费Gas
	GasTipCap *big.Int
	// Gas 价格
	GasPrice *big.Int
	// 前导交易nonce
	FrontNonce uint64
	// 交易对地址
	PairAddress common.Address
	// 交易对地址数组
	Path []common.Address
	// 输入代币储备量
	ReserveInput *big.Int
	// 输出代币储备量
	ReserveOutput *big.Int
}

type BuildBackRunTxParams struct {
	// 受害者原始交易
	VictimTx *types.Transaction
	// 受害者交易输入数量(ETH or Token)
	VictimInAmount *big.Int
	// 前导交易输入数量(ETH or Token)
	FrontInAmount *big.Int
	// 矿工小费Gas
	GasTipCap *big.Int
	// Gas 价格
	GasPrice *big.Int
	// 后导交易nonce
	BackNonce uint64
	// 交易对地址
	PairAddress common.Address
	// 交易对地址数组
	Path []common.Address
	// 输入代币储备量
	ReserveInput *big.Int
	// 输出代币储备量
	ReserveOutput *big.Int
}

type BuildApproveTxParams struct {
	// 矿工小费Gas
	GasTipCap *big.Int
	// Gas 价格
	GasPrice *big.Int
	// 授权交易nonce
	ApproveNonce uint64
	// 代币地址
	TokenAddress common.Address
	// 输入代币量
	AmountIn *big.Int
}

type ArbitrageProfitableParams struct {
	// 受害者原始交易
	VictimTx *types.Transaction
	// 受害者交易输入数量(ETH or Token)
	VictimInAmount *big.Int
	// 前导交易输入数量(ETH or Token)
	FrontInAmount *big.Int
	// 总共耗费的Gas
	TotalGas uint64
	// Gas 价格
	GasPrice *big.Int
	// 矿工小费Gas
	GasTipCap *big.Int
	// 后导交易nonce
	BackNonce uint64
	// 交易对地址
	PairAddress common.Address
	// 交易对地址数组
	Path []common.Address
	// 输入代币储备量
	ReserveInput *big.Int
	// 输出代币储备量
	ReserveOutput *big.Int
}

func NewSandwichBuilder(ethClient *client.EthClient, parser *TransactionParser, pk *ecdsa.PrivateKey, defaultGas uint64, fbClient *client.FlashbotClient) *SandwichBuilder {
	return &SandwichBuilder{
		ethClient:   ethClient,
		fbClient:    fbClient,
		parser:      parser,
		privateKey:  pk,
		DefaultGas:  defaultGas,
		FromAddress: crypto.PubkeyToAddress(pk.PublicKey),
	}
}

func (b *SandwichBuilder) Build(ctx context.Context, tx *types.Transaction) ([]*types.Transaction, error) {
	var (
		eg          errgroup.Group     // 并行操作
		gasPrice    *big.Int           // 每个gas的价格
		gasTipCap   *big.Int           // 矿工小费gas
		gasBaseFee  *big.Int           // 基础gas费用
		allowance   *big.Int           // 代币授权Router额度
		pairAddress common.Address     // 交易对地址
		cancel      context.CancelFunc // 超时退出
	)
	ctx, cancel = context.WithTimeout(ctx, ctxExpireTime)
	defer cancel()
	/***********************************前置操作***********************************/
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
	path := params["path"].([]common.Address)
	/***********************************前置操作***********************************/

	/***********************************并行减少时间***********************************/
	// 同步链上nonce
	eg.Go(func() error {
		return b.ethClient.SyncNonce(ctx, b.FromAddress)
	})

	// 获取当前gas price
	eg.Go(func() error {
		gp, err := b.ethClient.GetDynamicGasPrice(ctx)
		if err != nil {
			return err
		}
		gasPrice = gp
		return nil
	})

	// 获取建议矿工小费
	eg.Go(func() error {
		gtc, err := b.ethClient.SuggestGasTipCap(ctx)
		if err != nil {
			return err
		}
		gasTipCap = gtc
		return nil
	})

	// 获取区块基础费用
	eg.Go(func() error {
		header, err := b.ethClient.HeaderByNumber(ctx, nil)
		if err != nil {
			return err
		}
		gasBaseFee = header.BaseFee
		return nil
	})

	// 获取代币授权额度
	eg.Go(func() error {
		al, err := b.getAllowance(ctx, path[1], b.parser.routerAddress)
		if err != nil {
			return err
		}
		allowance = al
		return nil
	})

	// 获取交易对地址
	eg.Go(func() error {
		pa, err := b.getPairAddress(path[0], path[1])
		if err != nil {
			return err
		}
		pairAddress = pa
		return nil
	})

	// 获取余额
	eg.Go(func() error {
		ba, err := b.ethClient.BalanceAt(ctx, b.FromAddress, nil)
		if err != nil {
			return err
		}
		if ba.Int64() == 0 {
			return errors.New("not enough balance")
		}
		return nil
	})

	// 等待所有并行任务完成
	if err = eg.Wait(); err != nil {
		return nil, err
	}
	/***********************************并行减少时间***********************************/

	/***********************************交易前置准备***********************************/
	// 获取连续nonce
	//frontNonce, err := b.ethClient.GetSequentialNonce(ctx, b.FromAddress)
	frontNonce, err := b.ethClient.PendingNonceAt(ctx, b.FromAddress)
	if err != nil {
		return nil, err
	}
	backNonce := frontNonce + 1

	// 获取代币储备量
	reserveInput, reserveOutput, err := b.getPoolReserves(ctx, &pairAddress, path[0], path[1])
	if err != nil {
		return nil, err
	}
	// 获取受害者兑换参数数据
	swapParams, err := b.parseSwapParams(method, params)
	if err != nil {
		return nil, fmt.Errorf("解析目标交易参数失败: %w", err)
	}

	// 获取受害者、前导交易数量
	victimInAmount, frontInAmount, inputIsETH := tx.Value(), new(big.Int), path[0] == b.parser.wethAddress
	if !inputIsETH {
		// 从交易参数获取代币输入量
		victimInAmount = swapParams.AmountIn
	}
	// 计算前导交易量（受害者交易量的n%）
	frontInAmount = new(big.Int).Mul(victimInAmount, big.NewInt(frontRunRatio))
	frontInAmount.Div(frontInAmount, big.NewInt(100))
	/***********************************交易前置准备***********************************/

	/***********************************授权前后导交易***********************************/
	var (
		eg2          errgroup.Group // 并行减少等待时间
		needApprove  bool           // 是否需要授权
		approveNonce uint64         // 授权交易的nonce
	)
	if needApprove = allowance.Cmp(big.NewInt(0)) == 0; needApprove {
		approveNonce = frontNonce
		frontNonce++
		backNonce++
	}

	// 普通bundle: 不同Gas交易
	gasPriceRange, gasTipCapRange := b.generateGasRange(gasPrice, gasTipCap, gasBaseFee)
	for i := 0; i < len(gasPriceRange); i++ {
		k := i
		eg2.Go(func() error {
			bundle, err := b.buildBundleTx(context.Background(), BuildBundleTxParams{
				VictimTx:       tx,
				VictimInAmount: victimInAmount,
				FrontInAmount:  frontInAmount,
				GasTipCap:      gasTipCapRange[k],
				GasPrice:       gasPriceRange[k],
				ApproveNonce:   approveNonce,
				FrontNonce:     frontNonce,
				BackNonce:      backNonce,
				PairAddress:    pairAddress,
				Path:           path,
				ReserveInput:   reserveInput,
				ReserveOutput:  reserveOutput,
				NeedApprove:    needApprove,
			})
			if err != nil {
				log.Printf("[Fight] buildBundleTx failed:%+v", err)
				return err
			}
			b.sendToFlashbot(context.Background(), bundle)
			return nil
		})
	}

	// 增强bundle: 黑洞+三明治交易
	_ = eg2.Wait()
	return nil, nil
}

// 构建授权前后导交易、利润判断
func (b *SandwichBuilder) buildBundleTx(ctx context.Context, in BuildBundleTxParams) ([]*types.Transaction, error) {
	var (
		eg               errgroup.Group
		approveTx        *types.Transaction // 授权交易
		frontTx          *types.Transaction // 前导交易
		backTx           *types.Transaction // 后导交易
		frontEstimateGas uint64             // 前导交易建议Gas
		backEstimateGas  uint64             // 后导交易建议Gas
		needApprove      bool               // 是否需要授权
	)

	eg.Go(func() error {
		// 需要授权
		if in.NeedApprove {
			maxAmountIn := new(big.Int)
			maxAmountIn.SetString(maxApproveAmount, 16)
			at, err := b.buildApproveTx(ctx, BuildApproveTxParams{
				GasTipCap:    in.GasTipCap,
				GasPrice:     in.GasPrice,
				ApproveNonce: in.ApproveNonce,
				TokenAddress: in.Path[1],
				AmountIn:     maxAmountIn,
			})
			if err != nil {
				return fmt.Errorf("构建授权交易失败(%v)", err)
			}
			approveTx = at
		}
		return nil
	})

	eg.Go(func() error {
		// 前导交易
		ft, feg, err := b.buildFrontRunTx(ctx, BuildFrontRunTxParams{
			VictimTx:      in.VictimTx,
			FrontInAmount: in.FrontInAmount,
			GasTipCap:     in.GasTipCap,
			GasPrice:      in.GasPrice,
			FrontNonce:    in.FrontNonce,
			PairAddress:   in.PairAddress,
			Path:          in.Path,
			ReserveInput:  in.ReserveInput,
			ReserveOutput: in.ReserveOutput,
		})
		if err != nil {
			return err
		}
		frontTx, frontEstimateGas = ft, feg
		return nil
	})

	eg.Go(func() error {
		// 后导交易
		bt, err := b.buildBackRunTx(ctx, BuildBackRunTxParams{
			VictimTx:       in.VictimTx,
			VictimInAmount: in.VictimInAmount,
			FrontInAmount:  in.FrontInAmount,
			GasTipCap:      in.GasTipCap,
			GasPrice:       in.GasPrice,
			BackNonce:      in.BackNonce,
			PairAddress:    in.PairAddress,
			Path:           in.Path,
			ReserveInput:   in.ReserveInput,
			ReserveOutput:  in.ReserveOutput,
		})
		if err != nil {
			return err
		}
		backTx = bt
		return nil
	})

	// 等待所有并行任务完成
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	/***********************************授权前后导交易***********************************/

	/***********************************利润空间判断***********************************/
	// 由于后导交易的建议gas获取不到，这里采用前导交易增加滑点的方式
	backEstimateGas = CalculateUint64SlipPoint(frontEstimateGas, slipPointBackGasLimit)
	frontEstimateGas = CalculateUint64SlipPoint(frontEstimateGas, slipPointFrontGasLimit)
	totalGas := frontEstimateGas + backEstimateGas
	if needApprove && approveTx != nil {
		totalGas += approveTx.Gas()
	}
	//真实交易跟模拟利润差了1倍，真实交易实际扣了0.00003494ETH，计算出来的是0.000068ETH
	isProfitable, err := b.isArbitrageProfitable(ArbitrageProfitableParams{
		VictimTx:       in.VictimTx,
		VictimInAmount: in.VictimInAmount,
		FrontInAmount:  in.FrontInAmount,
		TotalGas:       totalGas,
		GasPrice:       in.GasPrice,
		GasTipCap:      in.GasTipCap,
		BackNonce:      in.BackNonce,
		PairAddress:    in.PairAddress,
		Path:           in.Path,
		ReserveInput:   in.ReserveInput,
		ReserveOutput:  in.ReserveOutput,
	})
	if err != nil {
		return nil, err
	}
	if !isProfitable {
		return nil, common2.ErrNotEnoughProfit
	}
	/***********************************利润空间判断***********************************/

	log.Println("[Fight]",
		"approveNonce", in.ApproveNonce,
		"frontNonce", in.FrontNonce,
		"backNonce", in.BackNonce,
		"token", in.Path[1],
		"gasPrice", in.GasPrice,
		"gasPriceETH", WeiToEth(in.GasPrice),
		"gasTipCap", in.GasTipCap,
		"gasTipCapETH", WeiToEth(in.GasTipCap),
	)
	if needApprove && approveTx != nil {
		//return []*types.Transaction{approveTx, frontTx, backTx}, nil
		return []*types.Transaction{approveTx, frontTx, in.VictimTx, backTx}, nil
	}
	//return []*types.Transaction{frontTx, backTx}, nil
	return []*types.Transaction{frontTx, in.VictimTx, backTx}, nil
}

// 发送到Flashbot机器人
func (b *SandwichBuilder) sendToFlashbot(ctx context.Context, bundle []*types.Transaction) {
	if err := b.fbClient.CallBundle(ctx, bundle); err != nil {
		log.Printf("\r\n\r\n\r\n[Fight] CallBundle failed: %v, bundle: %v", err, bundle)
		// TODO: [优化] 选择记录以太坊节点跟flashbot节点比较近的服务器？因为这里耗时最久
	} else if err := b.fbClient.EthSendBundle(ctx, bundle, flashbotRetryCount); err != nil {
		log.Printf("\r\n\r\n\r\n[Fight] sendBundle failed: %v", bundle)
	} else {
		log.Printf("\r\n\r\n\r\n[Fight] sendBundle success: %v", bundle)
	}
}

// 构建买入交易（前跑）
func (b *SandwichBuilder) buildFrontRunTx(ctx context.Context, in BuildFrontRunTxParams) (*types.Transaction, uint64, error) {
	const (
		methodPrefix = "buildFrontRunTx"
	)

	// 获取输入资产类型，是否ETH
	inputIsETH := in.Path[0] == b.parser.wethAddress

	/***********************************交易数量计算***********************************/
	// 模拟输入交易扣除的手续费，这里是0.3%
	effectiveInput := new(big.Int).Mul(in.FrontInAmount, big.NewInt(actualTradeRatio))
	effectiveInput.Div(effectiveInput, big.NewInt(1000))

	// 预期通过输入得到的代币数量
	frontTokenOut := CalculateOutputAmount(effectiveInput, in.ReserveInput, in.ReserveOutput)

	// 重新计算最小输出（基于前导量+负滑点）, 可以根据调用路由合约的 getAmountsOut 判断现在能接收到的价格是否合理
	minAmountOut := CalculateWithSlippageEx(frontTokenOut, -slipPointBuy)
	/***********************************交易数量计算***********************************/

	/***********************************构造交易数据***********************************/
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())
	var (
		data []byte
		err  error
	)
	if inputIsETH {
		// ETH兑换
		data, err = b.parser.uniswapABI.Pack(config.MethodSwapExactETHForTokensSupportingFeeOnTransferTokens,
			minAmountOut,  // 愿意接受的 最少能换到多少代币，少于会失败
			in.Path,       // 兑换代币的路径 [weth, token]
			b.FromAddress, // 接收代币的路径
			deadline,      // 过期时间
		)
	} else {
		// 代币兑换
		data, err = b.parser.uniswapABI.Pack(config.MethodSwapExactTokensForTokensSupportingFeeOnTransferTokens,
			in.FrontInAmount, // 花出去的 代币 A 的数量 TODO:这里好像不对，是否需要考虑eth-intoken?
			minAmountOut,     // 愿意接受的 最少能换到多少代币 B，少于会失败
			in.Path,          // 兑换代币的路径 [代币A, 代币B]
			b.FromAddress,    // 接收代币的地址
			deadline,         // 过期时间
		)
	}

	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 交易数据构造失败: %w", methodPrefix, err)
	}
	/***********************************构造交易数据***********************************/

	/***********************************估算Gas Limit***********************************/
	gasLimit := b.DefaultGas
	callMsg := ethereum.CallMsg{
		From:      b.FromAddress,
		To:        in.VictimTx.To(),
		Value:     big.NewInt(0), // 代币兑换代币，则为0
		GasTipCap: in.GasTipCap,  // 矿工Gas费用
		GasFeeCap: in.GasPrice,   // 总Gas费用上限
		Data:      data,
	}
	if inputIsETH {
		callMsg.Value = in.FrontInAmount // ETH兑换，则为value
	}
	estimatedGas, err := b.ethClient.EstimateGas(ctx, callMsg)
	if err != nil {
		// 错误就直接返回了，避免错误导致卡nonce
		return nil, 0, fmt.Errorf("[%s] 前导交易获取gas limit错误: %w", methodPrefix, err)
	}
	if estimatedGas > 0 {
		gasLimit = estimatedGas
	}
	/***********************************估算Gas Limit***********************************/

	/***********************************构建并签名交易***********************************/
	txInner := &types.DynamicFeeTx{
		ChainID:   b.ethClient.Config.ChainID,                            // 链ID（防跨链重放攻击）
		Nonce:     in.FrontNonce,                                         // 当前交易的nonce
		GasTipCap: in.GasTipCap,                                          // 每个 Gas 的「矿工小费」（优先费，直接支付给矿工），也就是Gas的价格，实际消耗Gas数量取决于网络，由于数量不可控，但是价格可控，因为可通过调节价格决定矿工拿到的费用
		GasFeeCap: in.GasPrice,                                           // 每个 Gas 的「最大总费用」（含基础费用 BaseFee + 矿工小费 TipCap），必须满足GasFeeCap ≥ BaseFee + GasTipCap
		Gas:       CalculateUint64SlipPoint(gasLimit, slipPointGasLimit), // 最大Gas限制（实际消耗 Gas ≤ 此值，未用完的 Gas 会退还）
		To:        in.VictimTx.To(),                                      // 目标合约地址（如 Uniswap Router）
		Value:     big.NewInt(0),                                         // 代币兑换代币，则为0
		Data:      data,                                                  // 交易调用数据（ABI 编码的合约方法）
	}
	if inputIsETH {
		txInner.Value = in.FrontInAmount // ETH兑换，则为value
	}

	signedTx, err := b.buildAndSignTx(txInner)
	if err != nil {
		return nil, 0, fmt.Errorf("[%s] 构建签名交易失败: %w", methodPrefix, err)
	}
	//fmt.Printf("[Fight] Front tx nonce:%+v gasPrice:%+v gas:%+v to:%+v value:%+v\n", txInner.Nonce, txInner.GasPrice, txInner.Gas, txInner.To, txInner.Value)
	/***********************************构建并签名交易***********************************/

	return signedTx, estimatedGas, nil
}

// 构建卖入交易（后跑）
func (b *SandwichBuilder) buildBackRunTx(ctx context.Context, in BuildBackRunTxParams) (*types.Transaction, error) {
	const (
		methodPrefix = "buildBackRunTx"
	)

	// 获取输出资产类型，是否ETH
	outputIsETH := in.Path[0] == b.parser.wethAddress

	/***********************************交易数量计算***********************************/
	// 模拟前导交易影响，即模拟前导输入得到输出
	frontEffective := new(big.Int).Mul(in.FrontInAmount, big.NewInt(actualTradeRatio))
	frontEffective.Div(frontEffective, big.NewInt(1000))
	frontTokenOut := CalculateOutputAmount(frontEffective, in.ReserveInput, in.ReserveOutput)
	// 模拟前导交易之后的weth储备量
	reserveAfterFrontWETH := new(big.Int).Add(in.ReserveInput, frontEffective)
	// 模拟前导交易之后的token储备量
	reserveAfterFrontToken := new(big.Int).Sub(in.ReserveOutput, frontTokenOut)

	// 模拟受害者交易影响
	victimEffective := new(big.Int).Mul(in.VictimInAmount, big.NewInt(actualTradeRatio))
	victimEffective.Div(victimEffective, big.NewInt(1000))
	victimTokenOut := CalculateOutputAmount(victimEffective, reserveAfterFrontWETH, reserveAfterFrontToken)

	// 受害者买入之后的储备量
	finalReserveWETH := new(big.Int).Add(reserveAfterFrontWETH, victimEffective)
	finalReserveToken := new(big.Int).Sub(reserveAfterFrontToken, victimTokenOut)

	// 计算后导交易输出
	backEffective := new(big.Int).Mul(frontTokenOut, big.NewInt(actualTradeRatio))
	backEffective.Div(backEffective, big.NewInt(1000))
	expectedETH := CalculateOutputAmount(backEffective, finalReserveToken, finalReserveWETH)

	// 动态滑点计算
	amountOutMin := CalculateWithSlippageEx(expectedETH, -slipPointSell)
	if amountOutMin.Cmp(common.Big0) <= 0 {
		return nil, fmt.Errorf("[%s] 无效滑点计算 预期ETH/InToken:%s", methodPrefix, expectedETH.String())
	}
	/***********************************交易数量计算***********************************/

	/***********************************构造交易数据***********************************/
	deadline := big.NewInt(time.Now().Add(expireTime).Unix())
	reversePath := ReversePath(in.Path)
	methodName := config.MethodSwapExactTokensForTokensSupportingFeeOnTransferTokens // 默认代币兑换代币
	if outputIsETH {
		methodName = config.MethodSwapExactTokensForETHSupportingFeeOnTransferTokens // 否则为卖出eth
	}
	data, err := b.parser.uniswapABI.Pack(methodName,
		frontTokenOut, // 花出去的 代币 A 的精确数量，使用前导获得的全部代币
		big.NewInt(0), // 愿意接受的 最少能换到多少 ETH
		reversePath,   // 交易路径[代币A, 代币B] [代币地址, WETH地址]
		b.FromAddress, // ETH接收地址
		deadline,      // 交易过期时间戳
	)
	if err != nil {
		return nil, fmt.Errorf("交易数据构造失败: %w", err)
	}
	/***********************************构造交易数据***********************************/

	/***********************************估算Gas Limit***********************************/
	gasLimit := b.DefaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, ethereum.CallMsg{
		From:      b.FromAddress,
		To:        in.VictimTx.To(),
		GasTipCap: in.GasTipCap, // 矿工Gas费用
		GasFeeCap: in.GasPrice,  // 总Gas费用上限
		Data:      data,
	})
	// TODO: [优化] 由于卖出交易必错，因为没有持有数量无法计算建议gas，这里可能会导致浪费很多gas，建议获取前导gas？
	// 处理gas估算错误
	if estimatedGas > 0 {
		gasLimit = estimatedGas
	}
	/***********************************估算Gas Limit***********************************/

	/***********************************构建并签名交易***********************************/
	txInner := &types.DynamicFeeTx{
		ChainID:   b.ethClient.Config.ChainID,
		Nonce:     in.BackNonce,
		GasTipCap: in.GasTipCap,
		GasFeeCap: in.GasPrice,
		Gas:       CalculateUint64SlipPoint(gasLimit, slipPointGasLimit),
		To:        in.VictimTx.To(),
		Data:      data,
	}
	signedTx, err := b.buildAndSignTx(txInner)
	if err != nil {
		return nil, err
	}
	//fmt.Printf("[Fight] Back tx nonce:%+v gasPrice:%+v gas:%+v to:%+v value:%+v\n", txInner.Nonce, txInner.GasPrice, txInner.Gas, txInner.To, txInner.Value)
	/***********************************构建并签名交易***********************************/

	return signedTx, nil
}

// 构建授权交易
func (b *SandwichBuilder) buildApproveTx(ctx context.Context, in BuildApproveTxParams) (*types.Transaction, error) {
	// 打包调用数据
	approveData, err := b.parser.erc20ABI.Pack("approve", b.parser.routerAddress, in.AmountIn)
	if err != nil {
		return nil, err
	}
	approveCallMsg := ethereum.CallMsg{
		From:      b.FromAddress,
		To:        &in.TokenAddress,
		GasTipCap: in.GasTipCap, // 矿工Gas费用
		GasFeeCap: in.GasPrice,  // 总Gas费用上限
		Data:      approveData,
	}

	// 估算Gas Limit
	gasLimit := b.DefaultGas
	estimatedGas, err := b.ethClient.EstimateGas(ctx, approveCallMsg)
	// 处理gas估算错误
	if estimatedGas > 0 {
		gasLimit = estimatedGas
	}

	// 创建交易
	txInner := &types.DynamicFeeTx{
		ChainID:   b.ethClient.Config.ChainID,
		Nonce:     in.ApproveNonce,
		GasTipCap: in.GasTipCap,
		GasFeeCap: in.GasPrice,
		Gas:       CalculateUint64SlipPoint(gasLimit, approveSlipPointGas),
		To:        &in.TokenAddress,
		Data:      approveData,
	}
	signedTx, err := b.buildAndSignTx(txInner)
	if err != nil {
		return nil, err
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

// 获取交易对地址
// 链接参考:https://docs.uniswap.org/contracts/v2/guides/smart-contract-integration/getting-pair-addresses
func (b *SandwichBuilder) getPairAddress(tokenA, tokenB common.Address) (common.Address, error) {
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

// 利润判断核心方法
// 矿工奖励gas！！！！！！
func (b *SandwichBuilder) isArbitrageProfitable(in ArbitrageProfitableParams) (bool, error) {
	//-------------------
	// 第一阶段：前导交易（买入）
	//-------------------
	// 应用买入手续费
	frontEffectiveIn := new(big.Int).Mul(in.FrontInAmount, big.NewInt(actualTradeRatio))
	frontEffectiveIn.Div(frontEffectiveIn, big.NewInt(1000))

	// 计算前导交易输出
	frontTokenOut := CalculateOutputAmount(
		frontEffectiveIn,
		in.ReserveInput,  // WETH储备
		in.ReserveOutput, // Token储备
	)

	// 更新储备量（买入方向：WETH增加，Token减少）
	reserveAfterFrontWETH := new(big.Int).Add(in.ReserveInput, frontEffectiveIn)
	reserveAfterFrontToken := new(big.Int).Sub(in.ReserveOutput, frontTokenOut)

	//-------------------
	// 第二阶段：受害者交易（买入）
	//-------------------
	// 应用买入手续费
	victimEffectiveIn := new(big.Int).Mul(in.VictimInAmount, big.NewInt(actualTradeRatio))
	victimEffectiveIn.Div(victimEffectiveIn, big.NewInt(1000))

	// 计算受害者输出
	victimTokenOut := CalculateOutputAmount(
		victimEffectiveIn,
		reserveAfterFrontWETH,  // 使用前导后的WETH作为输入储备
		reserveAfterFrontToken, // 使用前导后的Token作为输出储备
	)

	// 更新储备量（买入方向）
	reserveAfterVictimWETH := new(big.Int).Add(reserveAfterFrontWETH, victimEffectiveIn)
	reserveAfterVictimToken := new(big.Int).Sub(reserveAfterFrontToken, victimTokenOut)

	//-------------------
	// 第三阶段：尾随交易（卖出）
	//-------------------
	// 可卖出的最大Token量（考虑前导交易实际到账）
	availableTokenOut := new(big.Int).Mul(frontTokenOut, big.NewInt(actualTradeRatio))
	availableTokenOut.Div(availableTokenOut, big.NewInt(1000))

	// 计算尾随交易输出（卖出方向）
	backEthOut := CalculateOutputAmount(
		availableTokenOut,
		reserveAfterVictimToken, // 此时Token是输入储备
		reserveAfterVictimWETH,  // WETH是输出储备
	)

	//-------------------
	// 成本利润计算
	//-------------------
	// 总投入成本（前导交易金额 + Gas成本）
	totalCost := new(big.Int).Set(in.FrontInAmount)

	// Gas成本转换（假设GasPrice单位是Wei）
	gasCostWei := new(big.Int).Mul(
		big.NewInt(int64(in.TotalGas)),
		in.GasPrice,
	)
	totalCost.Add(totalCost, gasCostWei)

	// 最终利润
	profit := new(big.Int).Sub(backEthOut, totalCost)

	// 调试输出
	log.Printf(
		"[Profit-Fight] 代币:%v, GasPrice/TipCap:%v-%v 投入总成本:%s | 买入成本:%s | Gas成本:%s | 实际利润%s\n",
		in.Path[1],
		in.GasPrice,
		in.GasTipCap,
		WeiToEth(totalCost),
		WeiToEth(in.FrontInAmount),
		WeiToEth(gasCostWei),
		WeiToEth(profit),
	)

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

// 辅助方法：判断代币是否授权, 读取本地状态
func (b *SandwichBuilder) isTokenApproved(token common.Address) bool {
	_, ok := b.approveTokenMap.Load(token)
	return ok
}

// 辅助方法：代币授权标记, 写入本地状态
func (b *SandwichBuilder) setTokenApprove(token common.Address) {
	b.approveTokenMap.Store(token, true)
}

// 辅助方法：构建签名交易
func (b *SandwichBuilder) buildAndSignTx(txInner *types.DynamicFeeTx) (*types.Transaction, error) {
	tx := types.NewTx(txInner)
	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(txInner.ChainID), b.privateKey)
	if err != nil {
		return nil, fmt.Errorf("交易签名失败: %w", err)
	}
	return signedTx, nil
}

// 辅助方法：根据Gas最低最高构建Gas范围
func (b *SandwichBuilder) generateGasRange(gasPrice, gasTipCap, gasBaseFee *big.Int) ([]*big.Int, []*big.Int) {
	gasPriceRange, gasTipCapRange := make([]*big.Int, 0), make([]*big.Int, 0)

	// 从最低滑点开始，每次递增直到超过最高值
	for current := slipPointGasPriceMin; current <= slipPointGasPriceMax; current += slipPointIncreasePer {
		// 计算当前滑点下的值
		curGasTipCap := CalculateWithSlippageEx(gasTipCap, int(current))
		curGasPrice := CalculateWithSlippageEx(gasPrice, int(current))

		// 确保 gasPrice >= baseFee + tipCap
		minGasPrice := new(big.Int).Add(gasBaseFee, curGasTipCap)
		if curGasPrice.Cmp(minGasPrice) < 0 {
			curGasPrice = minGasPrice
		}
		gasTipCapRange = append(gasTipCapRange, curGasTipCap)
		gasPriceRange = append(gasPriceRange, curGasPrice)
	}
	return gasPriceRange, gasTipCapRange
}

func atomicIncrease(target *int32, rate, max int32) {
	for {
		old := atomic.LoadInt32(target)
		newVal := old + rate
		if newVal > max {
			newVal = max
		}
		if atomic.CompareAndSwapInt32(target, old, newVal) {
			break
		}
	}
}

// GetSlipPointGasPrice 并发安全获取 gas价格滑点
func GetSlipPointGasPrice() int {
	return int(atomic.LoadInt32(&slipPointGasPriceMin))
}

// IncreaseSlipPointGasPrice 安全递增
func IncreaseSlipPointGasPrice() {
	rate := atomic.LoadInt32(&slipPointIncreaseRate)
	maxVal := atomic.LoadInt32(&slipPointIncreaseMax)
	atomicIncrease(&slipPointGasPriceMin, rate, maxVal)
}

// GetSlipPointGasTipCap 并发安全获取矿工小费gas滑点
func GetSlipPointGasTipCap() int {
	return int(atomic.LoadInt32(&slipPointGasTipCapMin))
}

// IncreaseSlipPointGasTipCap 安全递增
func IncreaseSlipPointGasTipCap() {
	rate := atomic.LoadInt32(&slipPointIncreaseRate)
	maxVal := atomic.LoadInt32(&slipPointIncreaseMax)
	atomicIncrease(&slipPointGasTipCapMin, rate, maxVal)
}
