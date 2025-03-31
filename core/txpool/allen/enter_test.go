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
	//ethCli, err := ethclient.Dial("https://sepolia.infura.io/v3/7d5a6189348c438ab586891e09415578")
	//ethCli, err := ethclient.Dial("https://sepolia.infura.io/v3/7d5a6189348c438ab586891e09415578")
	//ethCli, err := ethclient.Dial("https://mainnet.infura.io/v3/0f498747340a49e498cfdaaa1fcb2f2a")
	//ethCli, err := ethclient.Dial("https://sepolia.infura.io/v3/0f498747340a49e498cfdaaa1fcb2f2a")
	ethCli, err := ethclient.Dial("https://sepolia.infura.io/v3/b7747070162f4fd698da299bf4e172a1")
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
	// eth-
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x9ea426a6df1f02adba83a43faf6f24b7761e2d478fead41b8c37dcdf64455434"))
	// eth-SECHT sepolia
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x97876cb6cea42546350fefe0c00d4be4516d382f0f21083d2598aa90b89cc32a"))
	// eth-USDC sepolia
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x93e4cb37a9fc55b22ca25163d61e40d1b4ae56e55db3653f07fff7528bee8563"))
	// eth-USDC sepolia
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x6990e40ab57325a7352e2437a10611a4fb8d0fb84af09de001a5a4872e92554a"))
	// eth-USDC sepolia
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0xab50d537965f50ed81980b12b9a26a5e25dbb1732557c6843c7afb52f649e1ef"))
	// eth-UNI sepolia
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x0ab8d8327f48b7358854f58ddfd9db835382f97970a502953a5680e88e8b4bae"))
	// eth-rgi，用来测试授权
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0xe2af59cf8a33fc7697c4420055ae62bb66750819ca6e5a027d867c3eacfb69dd"))
	// yu-btc
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x838e57cbe839b44be98131b5e7df3d86cd93ff19900b5bb6f0a8d81278cd2cbd"))
	// eth-BWINX, 主网
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0x4adbc33afea3c7f041c64d0f3f8807f5efb596d8e125bdec17429e31b93aa15d"))
	// eth-Dogestory, 主网
	//tx, _, err := ethCli.TransactionByHash(ctx, common.HexToHash("0xee3f71d256af6002385829400a487efecacc802c999d41a13563bedcbca1fd6b"))
	if err != nil {
		panic(err)
	}
	startTime := time.Now()
	fmt.Println("开始夹击，当前时间", startTime.UnixNano())
	Fight(tx)
	endTime := time.Now()
	fmt.Println("夹击完成，共耗时", endTime.UnixNano()-startTime.UnixNano(), "纳秒", endTime.UnixMilli()-startTime.UnixMilli(), "毫秒")
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
