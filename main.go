package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	zg_common "github.com/0gfoundation/0g-storage-client/common"
	"github.com/0gfoundation/0g-storage-client/common/blockchain"
	"github.com/0gfoundation/0g-storage-client/core"
	"github.com/0gfoundation/0g-storage-client/indexer"
	"github.com/0gfoundation/0g-storage-client/transfer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/joho/godotenv"
	"github.com/openweb3/web3go"
	"github.com/sirupsen/logrus"
)

// 全局配置结构体（与 .env 字段对应）
var cfg struct {
	// 0g 网络配置
	ChainURL    string
	IndexerURL  string
	PrivateKey  string

	// 文件路径配置
	InputFile   string
	OutputFile  string

	// 运行参数配置
	Timeout     time.Duration
	Routines    int
	WithProof   bool

	// 分片配置
	FragmentSize  int64
	TotalFileSize int64
}

func main() {
	logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	fmt.Println("=====================================")
	fmt.Println("📤 0g-storage 4GB 文件分片上传/下载（.env配置版）")
	fmt.Println("=====================================")

	// 1. 加载 .env 配置
	if err := loadEnvConfig(); err != nil {
		logrus.WithError(err).Fatal("❌ 加载配置失败")
	}

	// 2. 验证配置和源文件
	if err := validateConfig(); err != nil {
		logrus.WithError(err).Fatal("❌ 配置验证失败")
	}

	// 3. 创建上下文（带超时）
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	// 4. 初始化 web3 客户端
	w3client := blockchain.MustNewWeb3(cfg.ChainURL, cfg.PrivateKey)
	defer w3client.Close()
	logrus.Info("✅ web3 客户端初始化成功")

	// 5. 分片上传
	rootHashes, err := uploadFragments(ctx, w3client)
	if err != nil {
		logrus.WithError(err).Fatal("❌ 分片上传失败")
	}
	logrus.Infof("✅ 所有分片上传完成，共 %d 个分片，根哈希列表: %v", len(rootHashes), rootHashes)

	// 6. 下载并合并分片
	if err := downloadAndMergeFragments(ctx, rootHashes); err != nil {
		logrus.WithError(err).Fatal("❌ 下载合并失败")
	}

	logrus.Infof("🎉 所有流程完成！下载合并后的文件：%s", cfg.OutputFile)
	fmt.Println("=====================================")
}

// 加载 .env 配置到全局变量
func loadEnvConfig() error {
	logrus.Info("📥 加载 .env 配置文件...")

	// 加载 .env 文件
	if err := godotenv.Load(); err != nil {
		return fmt.Errorf("读取 .env 文件失败：%w（请确保 .env 文件在当前目录）", err)
	}

	// 网络配置
	cfg.ChainURL = getEnv("OG_CHAIN_URL", "")
	cfg.IndexerURL = getEnv("OG_INDEXER_URL", "")
	cfg.PrivateKey = getEnv("OG_PRIVATE_KEY", "")

	// 文件路径配置
	cfg.InputFile = getEnv("INPUT_FILE", "./4gb_test.bin")
	cfg.OutputFile = getEnv("OUTPUT_FILE", "./downloaded_4gb.bin")

	// 运行参数配置
	timeoutStr := getEnv("OP_TIMEOUT", "1h")
	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return fmt.Errorf("OP_TIMEOUT 格式错误：%w（支持 1h/30m 等格式）", err)
	}
	cfg.Timeout = timeout

	routinesStr := getEnv("CONCURRENT_ROUTINES", "4")
	routines, err := strconv.Atoi(routinesStr)
	if err != nil || routines < 1 || routines > 10 {
		return fmt.Errorf("CONCURRENT_ROUTINES 无效：%s（必须是 1-10 之间的整数）", routinesStr)
	}
	cfg.Routines = routines

	withProofStr := getEnv("DOWNLOAD_WITH_PROOF", "true")
	withProof, err := strconv.ParseBool(withProofStr)
	if err != nil {
		return fmt.Errorf("DOWNLOAD_WITH_PROOF 格式错误：%w（只能是 true/false）", err)
	}
	cfg.WithProof = withProof

	// 分片配置
	fragmentSizeStr := getEnv("FRAGMENT_SIZE", "419430400")
	fragmentSize, err := strconv.ParseInt(fragmentSizeStr, 10, 64)
	if err != nil {
		return fmt.Errorf("FRAGMENT_SIZE 格式错误：%w", err)
	}
	cfg.FragmentSize = fragmentSize

	totalFileSizeStr := getEnv("TOTAL_FILE_SIZE", "4294967296")
	totalFileSize, err := strconv.ParseInt(totalFileSizeStr, 10, 64)
	if err != nil {
		return fmt.Errorf("TOTAL_FILE_SIZE 格式错误：%w", err)
	}
	cfg.TotalFileSize = totalFileSize

	logrus.Info("✅ 配置加载完成")
	return nil
}

// 验证配置和源文件有效性
func validateConfig() error {
	logrus.Info("🔍 验证配置和源文件...")

	// 验证必填配置
	if cfg.ChainURL == "" {
		return fmt.Errorf("OG_CHAIN_URL 不能为空")
	}
	if cfg.IndexerURL == "" {
		return fmt.Errorf("OG_INDEXER_URL 不能为空")
	}
	if cfg.PrivateKey == "" || cfg.PrivateKey == "your_wallet_private_key_here" {
		return fmt.Errorf("请在 .env 中填写有效的 OG_PRIVATE_KEY（你的钱包私钥）")
	}

	// 验证源文件
	if err := checkFileSize(cfg.InputFile, cfg.TotalFileSize); err != nil {
		return fmt.Errorf("源文件验证失败：%w", err)
	}

	logrus.Info("✅ 配置和源文件验证通过")
	return nil
}

// 检查文件大小是否符合要求
func checkFileSize(path string, expectedSize int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("获取文件信息失败：%w（请确保文件存在）", err)
	}
	if info.Size() != expectedSize {
		return fmt.Errorf("文件大小异常：实际 %d bytes，预期 %d bytes（4GB）", info.Size(), expectedSize)
	}
	return nil
}

// 分片上传：将 4GB 文件切分为 10 个 400MB 分片并上传
func uploadFragments(ctx context.Context, w3client *web3go.Client) ([]common.Hash, error) {
	logrus.Info("📦 开始文件分片和上传...")

	// 打开源文件
	mainFile, err := core.Open(cfg.InputFile)
	if err != nil {
		return nil, fmt.Errorf("打开源文件失败：%w", err)
	}
	defer mainFile.Close()

	// 切分文件（按配置的分片大小）
	fragments := mainFile.Split(cfg.FragmentSize)
	logrus.Infof("✅ 文件切分完成，共生成 %d 个分片（每个 %d MB）", len(fragments), cfg.FragmentSize/1024/1024)

	// 初始化索引器客户端
	indexerClient, err := indexer.NewClient(cfg.IndexerURL, indexer.IndexerClientOption{
		LogOption: zg_common.LogOption{Logger: logrus.StandardLogger()},
	})
	if err != nil {
		return nil, fmt.Errorf("初始化索引器客户端失败：%w", err)
	}
	defer indexerClient.Close()

	rootHashes := make([]common.Hash, 0, len(fragments))

	// 逐个上传分片
	for i, fragment := range fragments {
		logrus.Infof("🚀 开始上传分片 %d", i)

		// 初始化上传器
		uploader, err := indexerClient.NewUploaderFromIndexerNodes(
			ctx,
			fragment.NumSegments(),
			w3client,
			1,  // 预期副本数
			nil,
			"min",  // 节点选择策略
			true,   // 使用全信任节点
		)
		if err != nil {
			return nil, fmt.Errorf("初始化分片 %d 上传器失败：%w", i, err)
		}
		uploader.WithRoutines(cfg.Routines)

		// 上传分片（配置 fragment size 适配 0g 要求）
		txHash, rootHash, err := uploader.Upload(ctx, fragment, transfer.UploadOption{
			TaskSize:         10,  // 单请求上传的段数
			FinalityRequired: transfer.TransactionPacked,  // 等待交易打包即可
			SkipTx:           false,
			NRetries:         3,  // 失败重试 3 次
		})
		if err != nil {
			return nil, fmt.Errorf("分片 %d 上传失败：%w", i, err)
		}

		rootHashes = append(rootHashes, rootHash)
		logrus.Infof("✅ 分片 %d 上传成功！交易哈希：%s，根哈希：%s", i, txHash.Hex(), rootHash.Hex())
	}

	return rootHashes, nil
}

// 下载所有分片并合并为原始文件
func downloadAndMergeFragments(ctx context.Context, rootHashes []common.Hash) error {
	logrus.Info("📥 开始下载并合并分片...")

	// 初始化索引器客户端
	indexerClient, err := indexer.NewClient(cfg.IndexerURL, indexer.IndexerClientOption{
		LogOption: zg_common.LogOption{Logger: logrus.StandardLogger()},
	})
	if err != nil {
		return fmt.Errorf("初始化索引器客户端失败：%w", err)
	}
	defer indexerClient.Close()

	// 创建输出文件
	outputFile, err := os.Create(cfg.OutputFile)
	if err != nil {
		return fmt.Errorf("创建输出文件失败：%w", err)
	}
	defer outputFile.Close()

	// 逐个下载分片并合并
	for i, rootHash := range rootHashes {
		logrus.Infof("📥 开始下载分片 %d（根哈希：%s）", i, rootHash.Hex())

		// 为每个分片创建独立下载器
		downloader, err := indexerClient.NewDownloaderFromIndexerNodes(ctx, rootHash.Hex())
		if err != nil {
			return fmt.Errorf("初始化分片 %d 下载器失败：%w", i, err)
		}
		downloader.WithRoutines(cfg.Routines)

		// 临时文件存储单个分片（下载完成后合并，最后自动删除）
		tempFile := fmt.Sprintf("%s_fragment_%d.tmp", cfg.OutputFile, i)
		defer os.Remove(tempFile)

		// 下载分片（可选验证 Merkle 证明）
		if err := downloader.Download(ctx, rootHash.Hex(), tempFile, cfg.WithProof); err != nil {
			return fmt.Errorf("分片 %d 下载失败：%w", i, err)
		}

		// 读取临时分片并合并到输出文件
		fragmentFile, err := os.Open(tempFile)
		if err != nil {
			return fmt.Errorf("打开分片 %d 临时文件失败：%w", i, err)
		}

		_, err = io.Copy(outputFile, fragmentFile)
		fragmentFile.Close()
		if err != nil {
			return fmt.Errorf("合并分片 %d 失败：%w", i, err)
		}

		logrus.Infof("✅ 分片 %d 下载并合并成功", i)
	}

	// 验证合并后文件大小
	if err := checkFileSize(cfg.OutputFile, cfg.TotalFileSize); err != nil {
		return fmt.Errorf("合并文件验证失败：%w", err)
	}

	return nil
}

// 从环境变量获取值，不存在则返回默认值
func getEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}