package tatakai

import (
	"bytes"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"math/big"
)

// CalculateUint64SlipPoint 辅助方法：计算uint64滑点
func CalculateUint64SlipPoint(input uint64, slipPoint int) uint64 {
	output := float64(input) * (100 + float64(slipPoint)) / 100.0
	return uint64(output)
}

// CalculateWithSlippageEx 增强版滑点计算（支持自定义精度）
// 参数:
// - amount: 原始金额
// - slipPoint: 滑点 (如 5 表示5%，-2 表示-2%)
// - precision: 精度基数 (默认100表示百分比，1000表示千分比)
func CalculateWithSlippageEx(amount *big.Int, slipPoint int) *big.Int {
	precision := 100 // 默认百分比精度

	base := big.NewInt(int64(precision))
	multiplier := big.NewInt(int64(precision + slipPoint))

	result := new(big.Int).Mul(amount, multiplier)
	result.Div(result, base)

	// 处理最小单位保护
	if result.Cmp(big.NewInt(0)) <= 0 {
		return big.NewInt(1)
	}
	return result
}

// ReversePath 反转交易路径
func ReversePath(path []common.Address) []common.Address {
	reversed := make([]common.Address, len(path))
	for i, v := range path {
		reversed[len(path)-1-i] = v
	}
	return reversed
}

// SortTokens 排序地址
func SortTokens(tokenA, tokenB common.Address) (common.Address, common.Address) {
	addrA := bytes.ToLower(tokenA.Bytes())
	addrB := bytes.ToLower(tokenB.Bytes())
	if bytes.Compare(addrA, addrB) < 0 {
		return tokenA, tokenB
	}
	return tokenB, tokenA
}

// CompareAddress 比较两个地址的大小
// result < 0: a 小于 b
// result > 0: a 大于 b
// result = 0: a 等于 b
func CompareAddress(a, b common.Address) int {
	return bytes.Compare(a[:], b[:]) // 直接转成切片后用 bytes.Compare
}

// WeiToEth wei转换成eth
func WeiToEth(wei *big.Int) string {
	if wei == nil {
		return "0.0000 ETH"
	}

	// 转换为ETH单位
	eth := new(big.Float).SetInt(wei)
	eth.Quo(eth, big.NewFloat(1e18)) // 1 ETH = 10^18 wei

	// 格式化为带符号的4位小数
	value, _ := eth.Float64()
	if value >= 0 {
		return fmt.Sprintf("+%.6f ETH", value)
	}
	return fmt.Sprintf("-%.6f ETH", -value) // 负号前置并取正值
}

// CalculateOutputAmount 辅助方法：Uniswap输出量计算
func CalculateOutputAmount(inputAmount, inputReserve, outputReserve *big.Int) *big.Int {
	numerator := new(big.Int).Mul(inputAmount, outputReserve)
	denominator := new(big.Int).Add(inputReserve, inputAmount)
	return new(big.Int).Div(numerator, denominator)
}
