package tatakai

import "errors"

var (
	ErrNotUniswapTx      = errors.New("not a uniswap transaction")
	ErrInvalidPath       = errors.New("invalid token path")
	ErrUnsupportedMethod = errors.New("unsupported method")
	ErrInvalidDataLen    = errors.New("tx date len invalid")
	ErrNotBuyMethod      = errors.New("not in buy method map")
)
