package allen

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool/allen/client"
	"github.com/ethereum/go-ethereum/core/txpool/allen/config"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"testing"
	"time"
)

func testInitAllen() *ethclient.Client {
	//ethCli, err := ethclient.Dial("wss://sepolia.infura.io/ws/v3/7d5a6189348c438ab586891e09415578")
	//ethCli, err := ethclient.Dial("https://sepolia.infura.io/v3/7d5a6189348c438ab586891e09415578")
	ethCli, err := ethclient.Dial("https://sepolia.infura.io/v3/0f498747340a49e498cfdaaa1fcb2f2a")
	if err != nil {
		panic(err)
	}
	Attack(ethCli)
	return ethCli
}

func TestMockFight(t *testing.T) {
	ethCli := testInitAllen()
	ctx := context.Background()
	// eth-link
	tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x6e2865505a20de3caea9845d92ab48024794b1197f3a936e814ced374d7b2dd1"))
	// eth-rgi，用来测试授权
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0xe2af59cf8a33fc7697c4420055ae62bb66750819ca6e5a027d867c3eacfb69dd"))
	// yu-btc
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x838e57cbe839b44be98131b5e7df3d86cd93ff19900b5bb6f0a8d81278cd2cbd"))
	if err != nil {
		panic(err)
	}
	startTime := time.Now()
	fmt.Println("开始夹击，当前时间", startTime.Unix())
	Fight(tx)
	fmt.Println("夹击完成，共耗时", time.Now().Unix()-startTime.Unix(), "秒")
}

func TestSpeedNonce(t *testing.T) {
	//ethCli := testInitAllen()
	ctx := context.Background()
	cfg := config.Get(config.Sepolia)
	ethCli, err := ethclient.Dial("wss://sepolia.infura.io/ws/v3/7d5a6189348c438ab586891e09415578")
	if err != nil {
		panic(err)
	}
	myEthClient, err := client.NewEthClient(cfg, ethCli)
	pk := loadPrivateKey("")
	for i := 19; i <= 19; i++ {
		speedNonce, err := myEthClient.SpeedNonce(ctx, pk, crypto.PubkeyToAddress(pk.PublicKey), uint64(i), 200)
		if err != nil {
			return
		}
		t.Logf("SpeedNonce:%+v", speedNonce)
	}
}
