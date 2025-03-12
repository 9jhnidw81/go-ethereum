package allen

import (
	"context"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"testing"
)

func testInitAllen() *ethclient.Client {
	ethCli, err := ethclient.Dial("wss://sepolia.infura.io/ws/v3/7d5a6189348c438ab586891e09415578")
	if err != nil {
		panic(err)
	}
	Attack(ethCli)
	return ethCli
}

func TestMockFight(t *testing.T) {
	ethCli := testInitAllen()
	ctx := context.Background()
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x6e2865505a20de3caea9845d92ab48024794b1197f3a936e814ced374d7b2dd1"))
	tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x0ee8fb2c8761258324d8e7cb63dc7af4b82a476832496f1ad0240f76131feb7f"))
	if err != nil {
		panic(err)
	}
	Fight(tx)
}
