package tatakai

import (
	"math/big"
	"testing"
)

func TestSandwichBuilder_generateGasRange(t *testing.T) {
	b := &SandwichBuilder{}
	gasPrice := big.NewInt(400)
	gasTipCap := big.NewInt(200)
	gasBaseFee := big.NewInt(100)
	got, got1 := b.generateGasRange(gasPrice, gasTipCap, gasBaseFee)
	t.Log(got, got1)
}
