package common

import "errors"

var (
	ErrNotUniswapTx      = errors.New("not a uniswap transaction")
	ErrNotUniswapBuyTx   = errors.New("not a uniswap buy transaction")
	ErrInvalidPath       = errors.New("invalid token path")
	ErrUnsupportedMethod = errors.New("unsupported method")
	ErrInvalidDataLen    = errors.New("tx date len invalid")
	ErrNotBuyMethod      = errors.New("not in buy method map")
	ErrNotEnoughProfit   = errors.New("not enough profit")
	ErrTxStuck           = errors.New("交易长时间未确认")
)
