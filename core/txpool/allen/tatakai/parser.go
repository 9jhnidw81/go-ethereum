package tatakai

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// 方法签名更新（包含更多需要处理的方法）
var uniswapMethods = map[string]string{
	"7ff36ab5": "swapExactETHForTokens",                              // 直接用ETH买入
	"38ed1739": "swapExactTokensForTokens",                           // 可以是卖出也可以是买入，需要额外判断, 固定输入量，求最大输出
	"8803dbee": "swapTokensForExactTokens",                           // 可以是卖出也可以是买入, 需要额外判断, 固定输出量，求最小输入
	"4a25d94a": "swapETHForExactTokens",                              // 有固定ETH换最多代币
	"b6f9de95": "swapExactETHForTokensSupportingFeeOnTransferTokens", // 将准确的ETH交换为代币支撑费用转移代币
}

// TransactionParser 交易解析器
type TransactionParser struct {
	routerAddress  common.Address
	wethAddress    common.Address
	factoryAddress common.Address
	uniswapABI     abi.ABI
	erc20ABI       abi.ABI
	pairABI        abi.ABI
}

func NewParser(routerAddr, wethAddr, factoryAddr, uniswapAbi, erc20Abi, pairAbi string) (*TransactionParser, error) {
	uniswapABI, err := abi.JSON(strings.NewReader(uniswapAbi))
	if err != nil {
		return nil, fmt.Errorf("解析UniswapABI失败: %v", err)
	}
	erc20ABI, err := abi.JSON(strings.NewReader(erc20Abi))
	if err != nil {
		return nil, fmt.Errorf("解析Erc20ABI失败: %v", err)
	}
	pairABI, err := abi.JSON(strings.NewReader(pairAbi))
	if err != nil {
		return nil, fmt.Errorf("解析PairABI失败: %v", err)
	}

	return &TransactionParser{
		routerAddress:  common.HexToAddress(routerAddr),
		wethAddress:    common.HexToAddress(wethAddr),
		factoryAddress: common.HexToAddress(factoryAddr),
		uniswapABI:     uniswapABI,
		erc20ABI:       erc20ABI,
		pairABI:        pairABI,
	}, nil
}

// ParseMethodAndParams 解析交易的方法和参数
func (p *TransactionParser) ParseMethodAndParams(tx *types.Transaction) (*abi.Method, map[string]interface{}, error) {
	const (
		methodPrefix = "ParseMethodAndParams"
	)

	if len(tx.Data()) < 4 {
		return nil, nil, ErrInvalidDataLen
	}
	methodData, txData := tx.Data()[:4], tx.Data()[4:]
	methodID := hex.EncodeToString(methodData)

	// 不是买入方法则直接退出过滤掉
	if _, exists := uniswapMethods[methodID]; !exists {
		return nil, nil, ErrNotBuyMethod
	}

	method, err := p.uniswapABI.MethodById(methodData)
	if err != nil {
		return nil, nil, ErrUnsupportedMethod
	}

	// 解析交易参数
	params := make(map[string]interface{})
	if err := method.Inputs.UnpackIntoMap(params, txData); err != nil {
		return nil, nil, fmt.Errorf("[%s] 解析交易参数失败: %v", methodPrefix, err)
	}
	return method, params, nil
}

// IsBuyTransaction 判断交易对地址是否满足
func (p *TransactionParser) IsBuyTransaction(params map[string]interface{}) (bool, error) {
	// 获取交易路径
	path, ok := params["path"].([]common.Address)
	if !ok || len(path) < 2 {
		return false, ErrInvalidPath
	}

	return path[0] == p.wethAddress, nil
}
