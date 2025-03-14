package tatakai

import (
	"encoding/hex"
	"fmt"
	common2 "github.com/ethereum/go-ethereum/core/txpool/allen/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
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
	routerAddress   common.Address
	wethAddress     common.Address
	inTokenAddress  common.Address
	factoryAddress  common.Address
	factoryAddress2 common.Address
	uniswapABI      abi.ABI
	erc20ABI        abi.ABI
	pairABI         abi.ABI
	factoryABI      abi.ABI
}

func NewParser(cfg *config.Config) (*TransactionParser, error) {
	uniswapABI, err := abi.JSON(strings.NewReader(cfg.RouterAbi))
	if err != nil {
		return nil, fmt.Errorf("解析UniswapABI失败: %v", err)
	}
	erc20ABI, err := abi.JSON(strings.NewReader(cfg.Erc20Abi))
	if err != nil {
		return nil, fmt.Errorf("解析Erc20ABI失败: %v", err)
	}
	pairABI, err := abi.JSON(strings.NewReader(cfg.PairAbi))
	if err != nil {
		return nil, fmt.Errorf("解析PairABI失败: %v", err)
	}
	factoryABI, err := abi.JSON(strings.NewReader(cfg.FactoryAbi))
	if err != nil {
		return nil, fmt.Errorf("解析FactoryABI失败: %v", err)
	}

	return &TransactionParser{
		routerAddress:   common.HexToAddress(cfg.RouterAddress),
		wethAddress:     common.HexToAddress(cfg.WETHAddress),
		inTokenAddress:  common.HexToAddress(cfg.InTokenAddress),
		factoryAddress:  common.HexToAddress(cfg.FactoryAddress),
		factoryAddress2: common.HexToAddress(cfg.FactoryAddress2),
		uniswapABI:      uniswapABI,
		erc20ABI:        erc20ABI,
		pairABI:         pairABI,
		factoryABI:      factoryABI,
	}, nil
}

// ParseMethodAndParams 解析交易的方法和参数
func (p *TransactionParser) ParseMethodAndParams(tx *types.Transaction) (*abi.Method, map[string]interface{}, error) {
	const (
		methodPrefix = "ParseMethodAndParams"
	)

	if len(tx.Data()) < 4 {
		return nil, nil, common2.ErrInvalidDataLen
	}
	methodData, txData := tx.Data()[:4], tx.Data()[4:]
	methodID := hex.EncodeToString(methodData)

	// 不是买入方法则直接退出过滤掉
	if _, exists := uniswapMethods[methodID]; !exists {
		return nil, nil, common2.ErrNotBuyMethod
	}

	method, err := p.uniswapABI.MethodById(methodData)
	if err != nil {
		return nil, nil, common2.ErrUnsupportedMethod
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
		return false, common2.ErrInvalidPath
	}

	return path[0] == p.wethAddress || path[0] == p.inTokenAddress, nil
}
