package main

/**
Source-Stats Summarizer takes the source-stats CSV files from the collector and summarizes them into a single CSV file.

Input: CSV file(s) with the following format:

	<timestamp_ms>,<tx_hash>,<source>

Output (currently):

	2023-08-30T20:34:47.253Z        INFO    Processed all input files       {"files": 22, "txTotal": "627,891", "memUsedMiB": "594"}
	2023-08-30T20:34:47.648Z        INFO    Overall tx count        {"infura": "578,606", "alchemy": "568,790", "ws://localhost:8546": "593,046", "blx": "419,725"}
	2023-08-30T20:34:47.696Z        INFO    Unique tx count {"blx": "22,403", "ws://localhost:8546": "9,962", "alchemy": "2,940", "infura": "4,658", "unique": "39,963 / 627,891"}
	2023-08-30T20:34:47.816Z        INFO    Not seen by local node  {"blx": "23,895", "infura": "9,167", "alchemy": "7,039", "notSeenByRef": "34,845 / 627,891"}

	Total unique tx: 627,891

	Transactions received:
	- alchemy: 			   568,790
	- blx:				   419,725
	- infura: 			   578,606
	- ws://localhost:8546: 593,046

	Unique tx (sole sender):
	- alchemy: 				2,940
	- blx: 					22,403
	- infura: 				4,658
	- ws://localhost:8546: 	9,962

	Transactions not seen by local node: 34,845 / 627,891
	- alchemy: 	7,039
	- blx: 		23,895
	- infura: 	9,167

more insight ideas?
- who sent first
*/

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flashbots/mempool-dumpster/common"
	"go.uber.org/zap"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

var (
	version = "dev" // is set during build process

	// Default values
	defaultDebug = os.Getenv("DEBUG") == "1"

	// Flags
	printVersion = flag.Bool("version", false, "only print version")
	debugPtr     = flag.Bool("debug", defaultDebug, "print debug output")

	outDirPtr  = flag.String("out", "", "where to save output files")
	outDatePtr = flag.String("out-date", "", "date to use in output file names")
	// limit = flag.Int("limit", 0, "max number of txs to process")

	// Errors
	// errLimitReached = errors.New("limit reached")

	// Helpers
	log     *zap.SugaredLogger
	printer = message.NewPrinter(language.English)
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Use: %s -out <output_directory> <input_file1> <input_file2> <input_dir>/*.csv ... \n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	// perhaps only print the version
	if *printVersion {
		fmt.Printf("mempool source-stats-summarizer %s\n", version)
		return
	}

	// Logger setup
	log = common.GetLogger(*debugPtr, false)
	defer func() { _ = log.Sync() }()

	// Collect input files from arguments
	files := flag.Args()
	if flag.NArg() == 0 {
		fmt.Println("No input files specified as arguments.")
		os.Exit(1)
	}

	log.Infow("Starting mempool-source-stats-summarizer", "version", version)

	// writing output file is optional
	if *outDirPtr != "" {
		log.Infof("Output directory: %s", *outDirPtr)

		// Prepare output file paths, and make sure they don't exist yet
		fnOut := getOutFilename()
		if _, err := os.Stat(fnOut); !errors.Is(err, os.ErrNotExist) {
			log.Fatalf("Output file already exists: %s", fnOut)
		}

		// Ensure the output directory exists
		err := os.MkdirAll(*outDirPtr, os.ModePerm)
		if err != nil {
			log.Errorw("os.MkdirAll", "error", err)
			return
		}
	}

	summarizeSourceStats(files)
}

func getOutFilename() string {
	fnOut := filepath.Join(*outDirPtr, "source-stats.csv")
	if *outDatePtr != "" {
		fnOut = filepath.Join(*outDirPtr, fmt.Sprintf("%s_source-stats.csv", *outDatePtr))
	}
	return fnOut
}

func checkInputFiles(files []string) {
	// Ensure all input files exist and are CSVs
	for _, filename := range files {
		s, err := os.Stat(filename)
		if errors.Is(err, os.ErrNotExist) {
			log.Fatalf("Input file does not exist: %s", filename)
		} else if err != nil {
			log.Fatalf("os.Stat: %s", err)
		}

		if s.IsDir() {
			log.Fatalf("Input file is a directory: %s", filename)
		} else if filepath.Ext(filename) != ".csv" {
			log.Fatalf("Input file is not a CSV file: %s", filename)
		}
	}
}

// summarizeSourceStats parses all input CSV files into one output CSV and one output Parquet file.
func summarizeSourceStats(files []string) { //nolint:gocognit
	checkInputFiles(files)

	txs := make(map[string]map[string]int64) // [hash][srcTag]timestampMs
	sources := make(map[string]bool)

	timestampFirst, timestampLast := int64(0), int64(0)
	cntProcessedFiles := 0
	cntProcessedRecords := int64(0)

	// Collect transactions from all input files to memory
	for _, filename := range files {
		log.Infof("Processing: %s", filename)
		cntProcessedFiles += 1
		cntTxInFileTotal := 0

		readFile, err := os.Open(filename)
		if err != nil {
			log.Errorw("os.Open", "error", err, "file", filename)
			return
		}
		defer readFile.Close()

		fileReader := bufio.NewReader(readFile)
		for {
			l, err := fileReader.ReadString('\n')
			if len(l) == 0 && err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				log.Errorw("fileReader.ReadString", "error", err)
				break
			}

			l = strings.Trim(l, "\n")
			items := strings.Split(l, ",") // timestamp,hash,source
			if len(items) != 3 {
				log.Errorw("invalid line", "line", l)
				continue
			}

			cntTxInFileTotal += 1

			ts, err := strconv.Atoi(items[0])
			if err != nil {
				log.Errorw("strconv.Atoi", "error", err, "line", l)
				continue
			}
			txTimestamp := int64(ts)
			txHash := items[1]
			txSrcTag := items[2]

			// that it's a valid hash
			if len(txHash) != 66 {
				log.Errorw("invalid hash length", "hash", txHash)
				continue
			}
			if _, err = hexutil.Decode(txHash); err != nil {
				log.Errorw("hexutil.Decode", "error", err, "line", l)
				continue
			}

			cntProcessedRecords += 1

			if timestampFirst == 0 || txTimestamp < timestampFirst {
				timestampFirst = txTimestamp
			}
			if txTimestamp > timestampLast {
				timestampLast = txTimestamp
			}

			// Add source to map
			sources[txSrcTag] = true

			// Add entry to txs map
			if _, ok := txs[txHash]; !ok {
				txs[txHash] = make(map[string]int64)
			}
			// hash already exists
			txs[txHash][txSrcTag] = txTimestamp
		}
		log.Infow("Processed file",
			"txInFile", printer.Sprintf("%d", cntTxInFileTotal),
			// "txNew", printer.Sprintf("%d", cntTxInFileNew),
			"txTotal", printer.Sprintf("%d", len(txs)),
			"memUsedMiB", printer.Sprintf("%d", common.GetMemUsageMb()),
		)
	}

	log.Infow("Processed all input files",
		"files", cntProcessedFiles,
		"records", printer.Sprintf("%d", cntProcessedRecords),
		"txTotal", printer.Sprintf("%d", len(txs)),
		"memUsedMiB", printer.Sprintf("%d", common.GetMemUsageMb()),
	)

	// Write output file
	if *outDirPtr != "" {
		err := writeTxCSV(txs)
		if err != nil {
			log.Errorw("writeTxCSV", "error", err)
		}
	}

	// Analyze
	analyzeTxs(timestampFirst, timestampLast, cntProcessedRecords, txs)
}

func writeTxCSV(txs map[string]map[string]int64) error {
	fn := getOutFilename()

	log.Infof("Output CSV file: %s", fn)
	f, err := os.OpenFile(fn, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Write header
	_, err = f.WriteString("timestamp_ms,hash,source\n")
	if err != nil {
		return err
	}

	// save tx+source by timestamp: [timestamp][hash] = source
	cache := make(map[int]map[string][]string)
	for hash, v := range txs {
		for srcTag, ts := range v {
			if _, ok := cache[int(ts)]; !ok {
				cache[int(ts)] = make(map[string][]string)
			}
			cache[int(ts)][hash] = append(cache[int(ts)][hash], srcTag)
		}
	}

	// sort by timestamp
	timestamps := make([]int, 0)
	for ts := range cache {
		timestamps = append(timestamps, ts)
	}
	sort.Ints(timestamps)

	// write to file
	for _, ts := range timestamps {
		for hash, srcTags := range cache[ts] {
			for _, srcTag := range srcTags {
				_, err = f.WriteString(fmt.Sprintf("%d,%s,%s\n", ts, hash, srcTag))
				if err != nil {
					return err
				}
			}
		}
	}

	log.Infof("Output file written: %s", fn)
	return nil
}

// Analyze
func analyzeTxs(timestampFirst, timestampLast, cntProcessedRecords int64, txs map[string]map[string]int64) { //nolint:gocognit
	numUniqueTxs := len(txs)

	// step 1: get overall tx / source
	srcCntOverallTxs := make(map[string]int64)
	for _, v := range txs {
		for srcTag := range v {
			srcCntOverallTxs[srcTag] += 1
		}
	}

	l := log
	for srcTag, cnt := range srcCntOverallTxs {
		l = l.With(srcTag, printer.Sprintf("%d", cnt))
	}
	l.Info("Overall tx count")

	// step 2: get unique tx / source
	srcCntUniqueTxs := make(map[string]int64)
	nUnique := 0
	for hash, v := range txs {
		if len(v) == 1 {
			for srcTag := range v {
				srcCntUniqueTxs[srcTag] += 1
				nUnique += 1
				_ = hash
				// fmt.Println("unique", srcTag, hash)
			}
		}
	}

	l = log
	for srcTag, cnt := range srcCntUniqueTxs {
		l = l.With(srcTag, printer.Sprintf("%d", cnt))
	}
	l.Infow("Unique tx count", "unique", printer.Sprintf("%d / %d", nUnique, numUniqueTxs))

	// step 3: get +/- vs reference
	ref := "ws://localhost:8546"
	srcNotSeenByRef := make(map[string]int64)
	nNotSeenByRef := 0
	for hash, v := range txs {
		if _, seenByRef := v[ref]; !seenByRef {
			nNotSeenByRef += 1
			for srcTag := range v {
				srcNotSeenByRef[srcTag] += 1
				_ = hash
				// fmt.Println("unique", srcTag, hash)
			}
		}
	}

	l = log
	for srcTag, cnt := range srcNotSeenByRef {
		l = l.With(srcTag, printer.Sprintf("%d", cnt))
	}
	l.Infow("Not seen by local node", "notSeenByRef", printer.Sprintf("%d / %d", nNotSeenByRef, numUniqueTxs))

	// How much earlier were transactions received by blx vs. the local node?
	blxFirst := 0
	blxG001Sec := 0
	blxG01Sec := 0
	blxG025Sec := 0
	blxG05Sec := 0
	blxG1Sec := 0
	blxG2Sec := 0
	blxG5Sec := 0

	for _, v := range txs {
		if len(v) == 1 {
			continue
		}

		if _, seenByBlx := v["blx"]; !seenByBlx {
			continue
		}
		if _, seenByRef := v[ref]; !seenByRef {
			continue
		}

		blxTS := v["blx"]
		refTS := v[ref]
		diff := blxTS - refTS
		if diff > 0 {
			blxFirst += 1
		}
		if diff > 10 {
			blxG001Sec += 1
		}
		if diff > 100 {
			blxG01Sec += 1
		}
		if diff > 250 {
			blxG025Sec += 1
		}
		if diff > 500 {
			blxG05Sec += 1
		}
		if diff > 1000 {
			blxG1Sec += 1
		}
		if diff > 2000 {
			blxG2Sec += 1
		}
		if diff > 5000 {
			blxG5Sec += 1
		}
	}

	log.Infow("Transactions received by blx before local node",
		"blxFirst", printer.Sprintf("%d", blxFirst),
		"blxG001Sec", printer.Sprintf("%d", blxG001Sec),
		"blxG01Sec", printer.Sprintf("%d", blxG01Sec),
		"blxG025Sec", printer.Sprintf("%d", blxG025Sec),
		"blxG05Sec", printer.Sprintf("%d", blxG05Sec),
		"blxG1Sec", printer.Sprintf("%d", blxG1Sec),
		"blxG2Sec", printer.Sprintf("%d", blxG2Sec),
		"blxG5Sec", printer.Sprintf("%d", blxG5Sec),
	)

	// convert timestamps to duration
	d := time.Duration(timestampLast-timestampFirst) * time.Millisecond
	t1 := time.Unix(timestampFirst/1000, 0).UTC()
	t2 := time.Unix(timestampLast/1000, 0).UTC()

	fmt.Println("")
	// fmt.Println("Input:")
	// fmt.Println("- Files:", printer.Sprintf("%d", cntProcessedFiles))
	// fmt.Printf("- From: %s \n- To:   %s \n- Duration: %s \n", t1.String(), t2.String(), d.String())
	fmt.Printf("- Records: %s \n", printer.Sprintf("%d", cntProcessedRecords))
	fmt.Printf("- From: %s \n", t1.String())
	fmt.Printf("- To:   %s \n", t2.String())
	fmt.Printf("        (%s) \n", d.String())
	// fmt.Printf("- Time:\n  - From: %s \n  -   To: %s \n  -  Dur: %s \n", t1.String(), t2.String(), d.String())
	fmt.Printf("\nUnique transactions: %s \n", printer.Sprintf("%d", numUniqueTxs))
	fmt.Println("")
	// fmt.Printf("Transactions received (%s total) \n", printer.Sprintf("%d", numUniqueTxs))
	fmt.Printf("Transactions received: \n")
	for srcTag, cnt := range srcCntOverallTxs {
		fmt.Printf("- %-20s %10s\n", srcTag, printer.Sprintf("%d", cnt))
	}
	// fmt.Printf("- total unique         %10s\n", )

	fmt.Println("")
	fmt.Println("Unique tx (sole sender):")
	for srcTag, cnt := range srcCntUniqueTxs {
		fmt.Printf("- %-20s %10s\n", srcTag, printer.Sprintf("%d", cnt))
	}

	fmt.Println("")
	fmt.Println("Transactions not seen by local node:", printer.Sprintf("%d / %d", nNotSeenByRef, numUniqueTxs))
	for srcTag, cnt := range srcNotSeenByRef {
		fmt.Printf("- %-20s %10s\n", srcTag, printer.Sprintf("%d", cnt))
	}

	fmt.Println("")
	fmt.Println("Bloxroute transactions received before local node:")
	fmt.Printf("- first: %8s (%s of %s blx txs) \n", printer.Sprintf("%d", blxFirst), printer.Sprintf("%d", srcCntOverallTxs["blx"]), common.Int64DiffPercentFmt(int64(blxFirst), srcCntOverallTxs["blx"]))
	fmt.Printf("- 10ms:  %8s \t %6s\n", printer.Sprintf("%d", blxG001Sec), common.Int64DiffPercentFmt(int64(blxG001Sec), srcCntOverallTxs["blx"]))
	fmt.Printf("- 100ms: %8s \t %6s\n", printer.Sprintf("%d", blxG01Sec), common.Int64DiffPercentFmt(int64(blxG01Sec), srcCntOverallTxs["blx"]))
	fmt.Printf("- 250ms: %8s \t %6s\n", printer.Sprintf("%d", blxG025Sec), common.Int64DiffPercentFmt(int64(blxG025Sec), srcCntOverallTxs["blx"]))
	fmt.Printf("- 500ms: %8s \t %6s\n", printer.Sprintf("%d", blxG05Sec), common.Int64DiffPercentFmt(int64(blxG05Sec), srcCntOverallTxs["blx"]))
	fmt.Printf("- 1sec:  %8s \t %6s\n", printer.Sprintf("%d", blxG1Sec), common.Int64DiffPercentFmt(int64(blxG1Sec), srcCntOverallTxs["blx"]))
	fmt.Printf("- 2sec:  %8s \t %6s\n", printer.Sprintf("%d", blxG2Sec), common.Int64DiffPercentFmt(int64(blxG2Sec), srcCntOverallTxs["blx"]))
	fmt.Printf("- 5sec:  %8s \t %6s\n", printer.Sprintf("%d", blxG5Sec), common.Int64DiffPercentFmt(int64(blxG5Sec), srcCntOverallTxs["blx"]))
}
