package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"
)

// Config holds bot configuration
type Config struct {
	BSCNodeURL         string
	MasterWalletAddr   string
	FollowerPrivateKey string
	CopyPercentage     float64
	TokenAddresses     []string
	RouterAddress      string
	WBNBAddress        string
	GasPrice           *big.Int
	GasLimit           uint64
	Testnet            bool
}

// CopyTradingBot handles copy trading on BSC
type CopyTradingBot struct {
	config          *Config
	client          *ethclient.Client
	followerKey     *ecdsa.PrivateKey
	followerAddress common.Address
	lastBlockNumber uint64
	processedTxs    map[string]bool
}

// PancakeSwap Router ABI (simplified - Swap event)
const routerABI = `[
	{
		"anonymous": false,
		"inputs": [
			{"indexed": true, "name": "sender", "type": "address"},
			{"indexed": false, "name": "amount0In", "type": "uint256"},
			{"indexed": false, "name": "amount1In", "type": "uint256"},
			{"indexed": false, "name": "amount0Out", "type": "uint256"},
			{"indexed": false, "name": "amount1Out", "type": "uint256"},
			{"indexed": true, "name": "to", "type": "address"}
		],
		"name": "Swap",
		"type": "event"
	},
	{
		"constant": false,
		"inputs": [
			{"name": "amountIn", "type": "uint256"},
			{"name": "amountOutMin", "type": "uint256"},
			{"name": "path", "type": "address[]"},
			{"name": "to", "type": "address"},
			{"name": "deadline", "type": "uint256"}
		],
		"name": "swapExactTokensForTokens",
		"outputs": [{"name": "amounts", "type": "uint256[]"}],
		"type": "function"
	},
	{
		"constant": false,
		"inputs": [
			{"name": "amountOutMin", "type": "uint256"},
			{"name": "path", "type": "address[]"},
			{"name": "to", "type": "address"},
			{"name": "deadline", "type": "uint256"}
		],
		"name": "swapExactETHForTokens",
		"outputs": [{"name": "amounts", "type": "uint256[]"}],
		"type": "function",
		"payable": true
	},
	{
		"constant": false,
		"inputs": [
			{"name": "amountIn", "type": "uint256"},
			{"name": "amountOutMin", "type": "uint256"},
			{"name": "path", "type": "address[]"},
			{"name": "to", "type": "address"},
			{"name": "deadline", "type": "uint256"}
		],
		"name": "swapExactTokensForETH",
		"outputs": [{"name": "amounts", "type": "uint256[]"}],
		"type": "function"
	}
]`

// ERC20 ABI (simplified)
const erc20ABI = `[
	{
		"constant": true,
		"inputs": [{"name": "_owner", "type": "address"}],
		"name": "balanceOf",
		"outputs": [{"name": "balance", "type": "uint256"}],
		"type": "function"
	},
	{
		"constant": false,
		"inputs": [
			{"name": "_spender", "type": "address"},
			{"name": "_value", "type": "uint256"}
		],
		"name": "approve",
		"outputs": [{"name": "", "type": "bool"}],
		"type": "function"
	},
	{
		"constant": true,
		"inputs": [],
		"name": "decimals",
		"outputs": [{"name": "", "type": "uint8"}],
		"type": "function"
	}
]`

func main() {
	// Load environment variables
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	config := loadConfig()

	// Initialize Ethereum client
	client, err := ethclient.Dial(config.BSCNodeURL)
	if err != nil {
		log.Fatalf("Failed to connect to BSC node: %v", err)
	}
	defer client.Close()

	// Load follower private key
	followerKey, err := crypto.HexToECDSA(strings.TrimPrefix(config.FollowerPrivateKey, "0x"))
	if err != nil {
		log.Fatalf("Invalid private key: %v", err)
	}

	followerAddress := crypto.PubkeyToAddress(followerKey.PublicKey)

	bot := &CopyTradingBot{
		config:          config,
		client:          client,
		followerKey:     followerKey,
		followerAddress: followerAddress,
		processedTxs:    make(map[string]bool),
	}

	// Get current block number
	header, err := client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatalf("Failed to get latest block: %v", err)
	}
	bot.lastBlockNumber = header.Number.Uint64()

	log.Println("🚀 BSC Copy Trading Bot Started")
	log.Printf("📡 Connected to BSC: %s", config.BSCNodeURL)
	log.Printf("👤 Master Wallet: %s", config.MasterWalletAddr)
	log.Printf("🤖 Follower Wallet: %s", followerAddress.Hex())
	log.Printf("📊 Copy Percentage: %.2f%%", config.CopyPercentage)
	log.Printf("🪙 Monitoring tokens: %v", config.TokenAddresses)
	log.Printf("🏪 Router: %s", config.RouterAddress)

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
	}()

	// Start monitoring
	bot.startMonitoring(ctx)
}

func loadConfig() *Config {
	testnet := getEnv("TESTNET", "false") == "true"

	var bscNodeURL string
	if testnet {
		bscNodeURL = getEnv("BSC_TESTNET_URL", "https://data-seed-prebsc-1-s1.binance.org:8545")
	} else {
		bscNodeURL = getEnv("BSC_NODE_URL", "https://bsc-dataseed1.binance.org")
	}

	gasPriceGwei := parseFloat(getEnv("GAS_PRICE_GWEI", "5"))
	gasPrice := new(big.Int).Mul(big.NewInt(int64(gasPriceGwei)), big.NewInt(1000000000)) // Convert to Wei

	return &Config{
		BSCNodeURL:         bscNodeURL,
		MasterWalletAddr:   getEnv("MASTER_WALLET_ADDRESS", ""),
		FollowerPrivateKey: getEnv("FOLLOWER_PRIVATE_KEY", ""),
		CopyPercentage:     parseFloat(getEnv("COPY_PERCENTAGE", "100")),
		TokenAddresses:     parseAddresses(getEnv("TOKEN_ADDRESSES", "")),
		RouterAddress:      getEnv("ROUTER_ADDRESS", "0x10ED43C718714eb63d5aA57B78B54704E256024E"), // PancakeSwap V2 Router
		WBNBAddress:        getEnv("WBNB_ADDRESS", "0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c"),
		GasPrice:           gasPrice,
		GasLimit:           parseUint64(getEnv("GAS_LIMIT", "300000")),
		Testnet:            testnet,
	}
}

func (bot *CopyTradingBot) startMonitoring(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second) // Check every 3 seconds
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bot.scanNewBlocks(ctx)
		}
	}
}

func (bot *CopyTradingBot) scanNewBlocks(ctx context.Context) {
	// Get latest block
	header, err := bot.client.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Printf("Error getting latest block: %v", err)
		return
	}

	latestBlock := header.Number.Uint64()

	// Scan from last processed block to latest
	startBlock := bot.lastBlockNumber + 1
	if startBlock > latestBlock {
		return
	}

	// Limit scan range to avoid too many blocks at once
	maxBlocks := uint64(10)
	endBlock := startBlock + maxBlocks - 1
	if endBlock > latestBlock {
		endBlock = latestBlock
	}

	log.Printf("Scanning blocks %d to %d", startBlock, endBlock)

	for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
		bot.scanBlock(ctx, blockNum)
	}

	bot.lastBlockNumber = endBlock
}

func (bot *CopyTradingBot) scanBlock(ctx context.Context, blockNumber uint64) {
	block, err := bot.client.BlockByNumber(ctx, big.NewInt(int64(blockNumber)))
	if err != nil {
		log.Printf("Error getting block %d: %v", blockNumber, err)
		return
	}

	masterAddr := common.HexToAddress(bot.config.MasterWalletAddr)
	routerAddr := common.HexToAddress(bot.config.RouterAddress)

	// Check each transaction in the block
	for _, tx := range block.Transactions() {
		// Skip if already processed
		txHash := tx.Hash().Hex()
		if bot.processedTxs[txHash] {
			continue
		}

		// Check if transaction is from master wallet
		from, err := bot.client.TransactionSender(ctx, tx, block.Hash(), 0)
		if err != nil {
			continue
		}

		if from != masterAddr {
			continue
		}

		// Check if transaction is to PancakeSwap router
		if tx.To() == nil || *tx.To() != routerAddr {
			continue
		}

		// Process the swap transaction
		bot.processSwapTransaction(ctx, tx, blockNumber)
		bot.processedTxs[txHash] = true
	}
}

func (bot *CopyTradingBot) processSwapTransaction(ctx context.Context, tx *types.Transaction, blockNumber uint64) {
	log.Printf("🔄 New swap detected from master wallet: %s", tx.Hash().Hex())

	// Get transaction receipt to see events
	receipt, err := bot.client.TransactionReceipt(ctx, tx.Hash())
	if err != nil {
		log.Printf("Error getting receipt: %v", err)
		return
	}

	// Parse router ABI
	routerABIParsed, err := abi.JSON(strings.NewReader(routerABI))
	if err != nil {
		log.Printf("Error parsing router ABI: %v", err)
		return
	}

	// Find Swap events
	for _, log := range receipt.Logs {
		if len(log.Topics) == 0 {
			continue
		}

		// Check if this is a Swap event (topic[0] is the event signature)
		swapEventSig := crypto.Keccak256Hash([]byte("Swap(address,uint256,uint256,uint256,uint256,address)"))
		if log.Topics[0] != swapEventSig {
			continue
		}

		// Parse swap event
		var swapEvent struct {
			Sender     common.Address
			Amount0In  *big.Int
			Amount1In  *big.Int
			Amount0Out *big.Int
			Amount1Out *big.Int
			To         common.Address
		}

		err = routerABIParsed.UnpackIntoInterface(&swapEvent, "Swap", log.Data)
		if err != nil {
			log.Printf("Error unpacking swap event: %v", err)
			continue
		}

		// Copy the swap
		bot.copySwap(ctx, tx, swapEvent, blockNumber)
	}
}

func (bot *CopyTradingBot) copySwap(ctx context.Context, originalTx *types.Transaction, swapEvent struct {
	Sender     common.Address
	Amount0In  *big.Int
	Amount1In  *big.Int
	Amount0Out *big.Int
	Amount1Out *big.Int
	To         common.Address
}, blockNumber uint64) {
	// Get transaction data
	txData := originalTx.Data()
	if len(txData) < 4 {
		return
	}

	// Parse router ABI to decode function
	routerABIParsed, err := abi.JSON(strings.NewReader(routerABI))
	if err != nil {
		log.Printf("Error parsing router ABI: %v", err)
		return
	}

	// Try to decode the method
	method, err := routerABIParsed.MethodById(txData[:4])
	if err != nil {
		log.Printf("Could not decode method: %v", err)
		return
	}

	log.Printf("📊 Swap method detected: %s", method.Name)

	// Decode the input parameters
	values, err := method.Inputs.Unpack(txData[4:])
	if err != nil {
		log.Printf("Error unpacking method inputs: %v", err)
		return
	}

	// Execute swap based on method type
	switch method.Name {
	case "swapExactETHForTokens":
		bot.executeETHForTokensSwap(ctx, values, blockNumber)
	case "swapExactTokensForETH":
		bot.executeTokensForETHSwap(ctx, values, blockNumber)
	case "swapExactTokensForTokens":
		bot.executeTokensForTokensSwap(ctx, values, blockNumber)
	default:
		log.Printf("⚠️  Unsupported swap method: %s", method.Name)
	}
}

func (bot *CopyTradingBot) executeETHForTokensSwap(ctx context.Context, values []interface{}, blockNumber uint64) {
	// Values: amountOutMin, path, to, deadline
	if len(values) < 4 {
		log.Printf("Invalid parameters for swapExactETHForTokens")
		return
	}

	amountOutMin := values[0].(*big.Int)
	path := values[1].([]common.Address)
	_ = values[2].(common.Address) // to - not used
	_ = values[3].(*big.Int)       // deadline - not used

	// Get master wallet balance to determine swap amount
	masterAddr := common.HexToAddress(bot.config.MasterWalletAddr)
	balance, err := bot.client.BalanceAt(ctx, masterAddr, nil)
	if err != nil {
		log.Printf("Error getting master balance: %v", err)
		return
	}

	// Calculate copy amount
	amountIn := new(big.Int).Mul(balance, big.NewInt(int64(bot.config.CopyPercentage)))
	amountIn.Div(amountIn, big.NewInt(100))

	if amountIn.Cmp(big.NewInt(0)) == 0 {
		log.Printf("Copy amount too small")
		return
	}

	// Check follower balance
	followerBalance, err := bot.client.BalanceAt(ctx, bot.followerAddress, nil)
	if err != nil {
		log.Printf("Error getting follower balance: %v", err)
		return
	}

	if followerBalance.Cmp(amountIn) < 0 {
		log.Printf("⚠️  Insufficient BNB balance. Need: %s, Have: %s", amountIn.String(), followerBalance.String())
		return
	}

	// Calculate minimum output with 5% slippage tolerance
	copyAmountOutMin := new(big.Int).Mul(amountOutMin, big.NewInt(int64(bot.config.CopyPercentage)))
	copyAmountOutMin.Div(copyAmountOutMin, big.NewInt(100))
	copyAmountOutMin.Mul(copyAmountOutMin, big.NewInt(95)) // 5% slippage
	copyAmountOutMin.Div(copyAmountOutMin, big.NewInt(100))

	// Set new deadline (current time + 20 minutes)
	newDeadline := big.NewInt(time.Now().Add(20 * time.Minute).Unix())

	log.Printf("🔄 Executing ETH->Tokens swap")
	log.Printf("   Amount In: %s BNB", weiToEther(amountIn))
	log.Printf("   Path: %v", path)
	log.Printf("   Min Out: %s", copyAmountOutMin.String())

	// Execute swap
	bot.callSwapExactETHForTokens(ctx, amountIn, copyAmountOutMin, path, bot.followerAddress, newDeadline)
}

func (bot *CopyTradingBot) executeTokensForETHSwap(ctx context.Context, values []interface{}, blockNumber uint64) {
	// Values: amountIn, amountOutMin, path, to, deadline
	if len(values) < 5 {
		log.Printf("Invalid parameters for swapExactTokensForETH")
		return
	}

	amountIn := values[0].(*big.Int)
	amountOutMin := values[1].(*big.Int)
	path := values[2].([]common.Address)
	_ = values[3].(common.Address) // to - not used
	_ = values[4].(*big.Int)       // deadline - not used

	// Calculate copy amount
	copyAmountIn := new(big.Int).Mul(amountIn, big.NewInt(int64(bot.config.CopyPercentage)))
	copyAmountIn.Div(copyAmountIn, big.NewInt(100))

	// Check token approval and balance
	tokenAddr := path[0]
	if err := bot.checkAndApproveToken(ctx, tokenAddr, copyAmountIn); err != nil {
		log.Printf("Error with token approval: %v", err)
		return
	}

	// Calculate minimum output
	copyAmountOutMin := new(big.Int).Mul(amountOutMin, big.NewInt(int64(bot.config.CopyPercentage)))
	copyAmountOutMin.Div(copyAmountOutMin, big.NewInt(100))
	copyAmountOutMin.Mul(copyAmountOutMin, big.NewInt(95))
	copyAmountOutMin.Div(copyAmountOutMin, big.NewInt(100))

	newDeadline := big.NewInt(time.Now().Add(20 * time.Minute).Unix())

	log.Printf("🔄 Executing Tokens->ETH swap")
	log.Printf("   Amount In: %s", copyAmountIn.String())
	log.Printf("   Path: %v", path)

	bot.callSwapExactTokensForETH(ctx, copyAmountIn, copyAmountOutMin, path, bot.followerAddress, newDeadline)
}

func (bot *CopyTradingBot) executeTokensForTokensSwap(ctx context.Context, values []interface{}, blockNumber uint64) {
	// Values: amountIn, amountOutMin, path, to, deadline
	if len(values) < 5 {
		log.Printf("Invalid parameters for swapExactTokensForTokens")
		return
	}

	amountIn := values[0].(*big.Int)
	amountOutMin := values[1].(*big.Int)
	path := values[2].([]common.Address)
	_ = values[3].(common.Address) // to - not used
	_ = values[4].(*big.Int)       // deadline - not used

	// Calculate copy amount
	copyAmountIn := new(big.Int).Mul(amountIn, big.NewInt(int64(bot.config.CopyPercentage)))
	copyAmountIn.Div(copyAmountIn, big.NewInt(100))

	// Check token approval and balance
	tokenAddr := path[0]
	if err := bot.checkAndApproveToken(ctx, tokenAddr, copyAmountIn); err != nil {
		log.Printf("Error with token approval: %v", err)
		return
	}

	// Calculate minimum output
	copyAmountOutMin := new(big.Int).Mul(amountOutMin, big.NewInt(int64(bot.config.CopyPercentage)))
	copyAmountOutMin.Div(copyAmountOutMin, big.NewInt(100))
	copyAmountOutMin.Mul(copyAmountOutMin, big.NewInt(95))
	copyAmountOutMin.Div(copyAmountOutMin, big.NewInt(100))

	newDeadline := big.NewInt(time.Now().Add(20 * time.Minute).Unix())

	log.Printf("🔄 Executing Tokens->Tokens swap")
	log.Printf("   Amount In: %s", copyAmountIn.String())
	log.Printf("   Path: %v", path)

	bot.callSwapExactTokensForTokens(ctx, copyAmountIn, copyAmountOutMin, path, bot.followerAddress, newDeadline)
}

func (bot *CopyTradingBot) checkAndApproveToken(ctx context.Context, tokenAddr common.Address, amount *big.Int) error {
	// Parse ERC20 ABI
	erc20ABIParsed, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return fmt.Errorf("error parsing ERC20 ABI: %v", err)
	}

	routerAddr := common.HexToAddress(bot.config.RouterAddress)

	// Check current allowance
	allowanceData, err := erc20ABIParsed.Pack("allowance", bot.followerAddress, routerAddr)
	if err != nil {
		return fmt.Errorf("error packing allowance call: %v", err)
	}

	// Call contract to get allowance
	callResult, callErr := bot.client.CallContract(ctx, ethereum.CallMsg{
		To:   &tokenAddr,
		Data: allowanceData,
	}, nil)
	if callErr == nil {
		// Parse result (simplified - would need proper ABI decoding)
		allowance := new(big.Int).SetBytes(callResult)
		if allowance.Cmp(amount) >= 0 {
			return nil // Already approved
		}
	}
	// If call fails or allowance insufficient, proceed with approval

	// Need to approve - use max uint256 for simplicity
	maxApproval := new(big.Int)
	maxApproval.SetString("115792089237316195423570985008687907853269984665640564039457584007913129639935", 10)

	approveData, err := erc20ABIParsed.Pack("approve", routerAddr, maxApproval)
	if err != nil {
		return fmt.Errorf("error packing approve call: %v", err)
	}

	// Create and send approval transaction
	nonce, err := bot.client.PendingNonceAt(ctx, bot.followerAddress)
	if err != nil {
		return fmt.Errorf("error getting nonce: %v", err)
	}

	chainID, err := bot.client.NetworkID(ctx)
	if err != nil {
		return fmt.Errorf("error getting chain ID: %v", err)
	}

	auth, err := bind.NewKeyedTransactorWithChainID(bot.followerKey, chainID)
	if err != nil {
		return fmt.Errorf("error creating transactor: %v", err)
	}

	auth.Nonce = big.NewInt(int64(nonce))
	auth.GasPrice = bot.config.GasPrice
	auth.GasLimit = 50000
	auth.Value = big.NewInt(0)

	tx := types.NewTransaction(auth.Nonce.Uint64(), tokenAddr, big.NewInt(0), auth.GasLimit, auth.GasPrice, approveData)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), bot.followerKey)
	if err != nil {
		return fmt.Errorf("error signing transaction: %v", err)
	}

	err = bot.client.SendTransaction(ctx, signedTx)
	if err != nil {
		return fmt.Errorf("error sending approval transaction: %v", err)
	}

	log.Printf("✅ Token approval sent: %s", signedTx.Hash().Hex())

	// Wait for approval confirmation (simplified - in production, wait for receipt)
	time.Sleep(3 * time.Second)

	return nil
}

func (bot *CopyTradingBot) callSwapExactETHForTokens(ctx context.Context, amountIn, amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) {
	routerABIParsed, _ := abi.JSON(strings.NewReader(routerABI))
	routerAddr := common.HexToAddress(bot.config.RouterAddress)

	swapData, err := routerABIParsed.Pack("swapExactETHForTokens", amountOutMin, path, to, deadline)
	if err != nil {
		log.Printf("Error packing swap data: %v", err)
		return
	}

	bot.sendSwapTransaction(ctx, routerAddr, amountIn, swapData)
}

func (bot *CopyTradingBot) callSwapExactTokensForETH(ctx context.Context, amountIn, amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) {
	routerABIParsed, _ := abi.JSON(strings.NewReader(routerABI))
	routerAddr := common.HexToAddress(bot.config.RouterAddress)

	swapData, err := routerABIParsed.Pack("swapExactTokensForETH", amountIn, amountOutMin, path, to, deadline)
	if err != nil {
		log.Printf("Error packing swap data: %v", err)
		return
	}

	bot.sendSwapTransaction(ctx, routerAddr, big.NewInt(0), swapData)
}

func (bot *CopyTradingBot) callSwapExactTokensForTokens(ctx context.Context, amountIn, amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) {
	routerABIParsed, _ := abi.JSON(strings.NewReader(routerABI))
	routerAddr := common.HexToAddress(bot.config.RouterAddress)

	swapData, err := routerABIParsed.Pack("swapExactTokensForTokens", amountIn, amountOutMin, path, to, deadline)
	if err != nil {
		log.Printf("Error packing swap data: %v", err)
		return
	}

	bot.sendSwapTransaction(ctx, routerAddr, big.NewInt(0), swapData)
}

func (bot *CopyTradingBot) sendSwapTransaction(ctx context.Context, to common.Address, value *big.Int, data []byte) {
	nonce, err := bot.client.PendingNonceAt(ctx, bot.followerAddress)
	if err != nil {
		log.Printf("Error getting nonce: %v", err)
		return
	}

	chainID, err := bot.client.NetworkID(ctx)
	if err != nil {
		log.Printf("Error getting chain ID: %v", err)
		return
	}

	tx := types.NewTransaction(nonce, to, value, bot.config.GasLimit, bot.config.GasPrice, data)

	signedTx, err := types.SignTx(tx, types.NewEIP155Signer(chainID), bot.followerKey)
	if err != nil {
		log.Printf("Error signing transaction: %v", err)
		return
	}

	err = bot.client.SendTransaction(ctx, signedTx)
	if err != nil {
		log.Printf("❌ Error sending swap transaction: %v", err)
		return
	}

	log.Printf("✅ Swap transaction sent: %s", signedTx.Hash().Hex())
}

func weiToEther(wei *big.Int) string {
	ether := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18))
	return ether.Text('f', 8)
}

// Helper functions
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func parseAddresses(s string) []string {
	if s == "" {
		return []string{}
	}
	addrs := strings.Split(s, ",")
	var result []string
	for _, addr := range addrs {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			result = append(result, addr)
		}
	}
	return result
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0
	}
	return f
}

func parseUint64(s string) uint64 {
	if s == "" {
		return 0
	}
	var u uint64
	_, err := fmt.Sscanf(s, "%d", &u)
	if err != nil {
		return 0
	}
	return u
}
