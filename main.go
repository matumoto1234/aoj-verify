package main

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/matumoto1234/aoj-verify/stopwatch"
)

func main() {
	filename := os.Args[1]

	annotation, err := readAnnotationInFile(filename)
	if err != nil {
		log.Fatal(err)
	}

	// テストケースダウンロード編
	problemID, err := extractProblemID(annotation.ProblemURL)
	if err != nil {
		log.Fatal(err)
	}

	testcasesHeaderResponse, err := fetchProblemTestcasesHeader(problemID)
	if err != nil {
		log.Fatal(err)
	}

	cacheDir := constructCacheDirPath(annotation.ProblemURL)

	var multiErr error

	for _, h := range testcasesHeaderResponse.Headers {
		apiURL := fmt.Sprintf("https://judgedat.u-aizu.ac.jp/testcases/%s/%d", problemID, h.Serial)

		if isTestcaseCached(cacheDir, h.Name) {
			continue
		}

		err := fetchTestcaseAndSaveToFile(apiURL, cacheDir, h.Name)
		if err != nil {
			multiErr = errors.Join(multiErr, err)
		}

		time.Sleep(3 * time.Second)
	}

	if multiErr != nil {
		log.Fatal(multiErr)
	}

	// Verify編
	err = verify(cacheDir, filename)
	if err != nil {
		log.Fatal(err)
	}
}

type runStatus int

const (
	unknown runStatus = iota
	accepted
	wrongAnswer
	runtimeError
	timeLimitExceeded
)

type runResult struct {
	testcaseName string
	status       runStatus
	execTime     time.Duration
}

func newRunResult(testcaseName string, status runStatus, execTime time.Duration) *runResult {
	return &runResult{
		testcaseName: testcaseName,
		status:       status,
		execTime:     execTime,
	}
}

func verify(cacheDir, buildFilename string) error {
	// tmp作って〜
	tmpDir, err := os.MkdirTemp(".aoj-verify", "tmp")
	if err != nil {
		return fmt.Errorf("failed to temporally directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	binaryFilepath := filepath.Join(tmpDir, "main")

	// Goファイルをビルドして〜
	var buildCmdStdErr bytes.Buffer
	buildCmd := exec.Command("go", "build", "-o", binaryFilepath, buildFilename)
	buildCmd.Stderr = &buildCmdStdErr

	err = buildCmd.Run()
	if err != nil {
		return fmt.Errorf("failed to build go file: %w\n%s", err, buildCmdStdErr.String())
	}

	// .in を取得して〜
	var inFilepaths []string
	err = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".in") {
			inFilepaths = append(inFilepaths, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk dir: %w", err)
	}

	slices.Sort(inFilepaths)

	var multiErr error
	var runResults []*runResult

	for _, inFilepath := range inFilepaths {
		// 標準入力に入力ケース渡して実行 & その標準出力と出力ケースを比較してジャッジ
		base := strings.TrimSuffix(inFilepath, ".in")
		outFilepath := base + ".out"

		inFile, err := os.Open(inFilepath)
		if err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed to read .in file: %w", err))
			continue
		}

		answerFilepath := filepath.Join(tmpDir, "answer"+rand.Text())
		answerFile, err := os.Create(answerFilepath)
		if err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed to create answer file: %w", err))
			continue
		}

		// run
		runCmd := exec.Command(binaryFilepath)
		runCmd.Stdin = inFile
		runCmd.Stdout = answerFile

		var stopwatch stopwatch.Stopwatch
		stopwatch.Start()

		err = runCmd.Run()

		elapsed := stopwatch.Elapsed()

		if err != nil {
			slog.Info("RE", slog.String("testcase", base), slog.Any("time", elapsed))
			runResults = append(runResults, newRunResult(base, runtimeError, elapsed))
			continue
		}

		// TODO: defer
		inFile.Close()
		answerFile.Close()

		// compare output
		equal, err := filesAreEqual(answerFilepath, outFilepath)
		if err != nil {
			multiErr = errors.Join(multiErr, fmt.Errorf("failed to compare files: %w", err))
			continue
		}

		if equal {
			slog.Info("AC", slog.String("testcase", base), slog.Any("time", elapsed))
			runResults = append(runResults, newRunResult(base, accepted, elapsed))
		} else {
			slog.Info("WA", slog.String("testcase", base), slog.Any("time", elapsed))
			runResults = append(runResults, newRunResult(base, wrongAnswer, elapsed))
		}
	}
	if multiErr != nil {
		return fmt.Errorf("failed to run case: %w", multiErr)
	}

	// print summary
	var slowestTime time.Duration
	var slowestTestcaseName string
	var acCount, waCount, tleCount, reCount int
	for _, v := range runResults {
		if slowestTime < v.execTime {
			slowestTime = v.execTime
			slowestTestcaseName = v.testcaseName
		}

		switch v.status {
		case accepted:
			acCount++
		case wrongAnswer:
			waCount++
		case timeLimitExceeded:
			tleCount++
		case runtimeError:
			reCount++
		}
	}

	slog.Info("summary",
		slog.Duration("slowest time", slowestTime),
		slog.String("slowest case", slowestTestcaseName),
		slog.Int("AC count", acCount),
		slog.Int("WA count", waCount),
		slog.Int("TLE count", tleCount),
		slog.Int("RE count", reCount),
	)

	return nil
}

func filesAreEqual(path1, path2 string) (bool, error) {
	f1, err := os.Open(path1)
	if err != nil {
		return false, err
	}
	defer f1.Close()

	f2, err := os.Open(path2)
	if err != nil {
		return false, err
	}
	defer f2.Close()

	b1 := new(bytes.Buffer)
	b2 := new(bytes.Buffer)

	if _, err := io.Copy(b1, f1); err != nil {
		return false, err
	}
	if _, err := io.Copy(b2, f2); err != nil {
		return false, err
	}

	return bytes.Equal(b1.Bytes(), b2.Bytes()), nil
}

func constructCacheDirPath(problemURL string) string {
	md5URL := md5.Sum([]byte(problemURL))
	md5URLStr := fmt.Sprintf("%x", md5URL)

	// TODO: .aoj-verify はオプションで指定できる文字列にする
	return filepath.Join(".aoj-verify", "cache", md5URLStr, "test")
}

func isTestcaseCached(dir, testcaseName string) bool {
	in := filepath.Join(dir, testcaseName+".in")
	return existsFileOrDir(in)
}

type testcase struct {
	ProblemID string `json:"problemId"`
	Serial    int    `json:"serial"`
	In        string `json:"in"`
	Out       string `json:"out"`
}

func existsFileOrDir(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fetchTestcaseAndSaveToFile(apiURL, dir, filename string) error {
	resp, err := http.Get(apiURL)
	if err != nil {
		return fmt.Errorf("failed to fetch testcases: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	var testcase testcase
	err = json.Unmarshal(body, &testcase)
	if err != nil {
		return fmt.Errorf("failed to unmarshal body: %w", err)
	}

	if !existsFileOrDir(dir) {
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			return fmt.Errorf("failed to mkdir: %w", err)
		}
	}

	inPath := filepath.Join(dir, filename+".in")
	in, err := os.Create(inPath)
	if err != nil {
		return fmt.Errorf("failed to create .in case: %w", err)
	}
	defer in.Close()
	_, err = io.Copy(in, strings.NewReader(testcase.In))

	outPath := filepath.Join(dir, filename+".out")
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("failed to create .out case: %w", err)
	}
	defer out.Close()
	_, err = io.Copy(out, strings.NewReader(testcase.Out))

	slog.Info("download and saved", slog.String("in", inPath), slog.String("out", outPath))
	return nil
}

type header struct {
	Serial     int    `json:"serial"`
	Name       string `json:"name"`
	InputSize  int    `json:"inputSize"`
	OutputSize int    `json:"outputSize"`
	Score      int    `json:"score"`
}

// Ref: http://developers.u-aizu.ac.jp/api?key=judgedat%2Ftestcases%2F%7BproblemId%7D%2Fheader_GET
type testcasesHeaderResponse struct {
	ProblemID string    `json:"problemId"`
	Headers   []*header `json:"headers"`
}

func fetchProblemTestcasesHeader(problemID string) (*testcasesHeaderResponse, error) {
	apiURL := fmt.Sprintf("https://judgedat.u-aizu.ac.jp/testcases/%s/header", problemID)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	header := &testcasesHeaderResponse{}
	err = json.Unmarshal(body, &header)
	if err != nil {
		return nil, err
	}

	return header, nil
}

func extractProblemID(problemURL string) (string, error) {
	u, err := url.Parse(problemURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse problemURL: %w", err)
	}

	switch u.Host {
	case "judge.u-aizu.ac.jp":
		// e.g. https://judge.u-aizu.ac.jp/onlinejudge/description.jsp?id=ALDS1_14_A
		query := u.Query()
		return query.Get("id"), nil

	case "onlinejudge.u-aizu.ac.jp":
		// e.g. https://onlinejudge.u-aizu.ac.jp/courses/lesson/1/ALDS1/14/ALDS1_14_A

		segments := strings.Split(u.Path, "/")
		return segments[len(segments)-1], nil
	default:
		errMsg := fmt.Sprintf("unsupported url. url: %s", problemURL)
		return "", errors.New(errMsg)
	}

	// unreached
}

func readAnnotationInFile(filename string) (*Annotation, error) {
	body, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	bodyStr := string(body)
	for line := range strings.Lines(bodyStr) {
		if !isAnnotationComment(line) {
			continue
		}

		a, err := readAnnotationComment(line)
		if err != nil {
			return nil, fmt.Errorf("failed to read annotation comment: %w", err)
		}

		return a, nil
	}

	errMsg := fmt.Sprintf("annotation comment is not found. filename: %s", filename)
	return nil, errors.New(errMsg)
}

type Annotation struct {
	ProblemURL string
}

func isAnnotationComment(line string) bool {
	return strings.HasPrefix(line, "// verification-helper: ")
}

func readAnnotationComment(comment string) (*Annotation, error) {
	annotationRegexp := regexp.MustCompile("// verification-helper: PROBLEM (.*)")

	matches := annotationRegexp.FindStringSubmatch(comment)
	if matches == nil {
		errMsg := fmt.Sprintf(`annotation comment is not match "// verification-helper: PROBLEM (.*)" comment: %s`, comment)
		return nil, errors.New(errMsg)
	}

	if len(matches) != 2 {
		errMsg := fmt.Sprintf(`annotation comment is not match "// verification-helper: PROBLEM (.*)" comment: %s`, comment)
		return nil, errors.New(errMsg)
	}

	return &Annotation{
		ProblemURL: matches[1],
	}, nil
}
