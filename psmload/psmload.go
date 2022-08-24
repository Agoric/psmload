package psmload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gammazero/workerpool"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/schollz/progressbar/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type PSMLoadApp struct {
	Instagoric string
	Workers    int64
	Fee        float64
	homeDir    string
	keys       []string
	chainId    string
	boardId    string
	mtx        sync.Mutex
	client     *retryablehttp.Client
}

func NewPSMLoadApp(instagoric string, workers int64, fee float64) *PSMLoadApp {
	rc := retryablehttp.NewClient()
	rc.RetryMax = 10
	rc.HTTPClient.Timeout = 2 * time.Minute
	rc.RetryWaitMin = time.Second * 3
	stdLog, _ := zap.NewStdLogAt(zap.L(), zapcore.DebugLevel)
	rc.Logger = stdLog
	return &PSMLoadApp{
		Instagoric: instagoric,
		Workers:    workers,
		Fee:        fee,
		homeDir:    filepath.Join(os.TempDir(), fmt.Sprintf("agoric_%d", time.Now().UnixNano())),
		keys:       make([]string, workers),
		client:     rc,
	}
}

func keyName(idx int64) string {
	return fmt.Sprintf("key%d", idx)
}

var re = regexp.MustCompile(`(?m)address: (agoric1.+)$`)
var reChain = regexp.MustCompile(`(?m)Chain: (.+?)$`)
var txChain = regexp.MustCompile(`(?m)txhash: (.+?)$`)

func (p *PSMLoadApp) newKey(idx int64) (string, string, error) {
	keyname := keyName(idx)
	out, err := exec.Command("agd", "keys", "add", keyname, "--keyring-backend", "test", "--home", p.homeDir, "--no-backup").Output()

	if err != nil {
		zap.L().Error("error creating key", zap.Error(err), zap.ByteString("output", out))
	}

	for _, match := range re.FindAllStringSubmatch(string(out), -1) {

		return match[1], keyname, nil
	}

	return "", "", fmt.Errorf("error making key")
}

func (p *PSMLoadApp) CreateKeys() error {
	for i := int64(0); i < p.Workers; i++ {
		r := i
		key, name, err := p.newKey(r)
		p.mtx.Lock()
		p.keys[i] = key
		p.mtx.Unlock()
		if err != nil {
			panic(err)
		}
		zap.L().Info("created key", zap.String("name", name), zap.String("key", key))
	}
	return nil
}

func (p *PSMLoadApp) instagoricToService(service string) string {
	return strings.ReplaceAll(p.Instagoric, ".agoric.net", fmt.Sprintf(".%s.agoric.net", service))
}
func (p *PSMLoadApp) provisionAddress(address string) error {
	endpoint := p.instagoricToService("faucet")
	data := url.Values{}
	data.Add("address", address)
	data.Add("command", "client")
	data.Add("clientType", "SMART_WALLET")
	resp, err := p.client.PostForm(fmt.Sprintf("%s/go", endpoint), data)
	if err != nil {
		zap.L().Error("error provisioning address", zap.Error(err))
		return fmt.Errorf("error provisioning")
	}
	defer resp.Body.Close()
	return nil
}
func (p *PSMLoadApp) ProvisionKeys() error {
	wp := workerpool.New(int(p.Workers))
	bar := progressbar.Default(p.Workers)

	for i := int64(0); i < p.Workers; i++ {
		r := i
		wp.Submit(func() {
			for {
				err := p.provisionAddress(p.keys[r])
				if err != nil {
					zap.L().Error("error provisioning", zap.Error(err))
					continue
				} else {
					bar.Add(1)
					return
				}
			}
		})
	}

	wp.StopWait()
	bar.Finish()
	return nil
}

func (p *PSMLoadApp) getChainId() (string, error) {
	chainInfo, err := p.client.Get(p.Instagoric)
	if err != nil {
		return "", err
	}
	defer chainInfo.Body.Close()
	bod, _ := ioutil.ReadAll(chainInfo.Body)
	for _, match := range reChain.FindAllStringSubmatch(string(bod), -1) {
		return match[1], nil
	}
	return "", fmt.Errorf("unable to find chain id")
}
func (p *PSMLoadApp) invokeNode(nodeArgs ...string) (string, error) {
	endpoint := p.instagoricToService("rpc")
	if p.chainId == "" {
		p.mtx.Lock()
		achainId, err := p.getChainId()
		if err != nil {
			p.mtx.Unlock()
			return "", err
		}
		p.chainId = achainId
		p.mtx.Unlock()
	}
	networkFlag := fmt.Sprintf(`{"rpc": "%s", "chainId": "%s"}`, endpoint, p.chainId)
	ourArgs := []string{"psm-tool.js", "--net", networkFlag}
	ourArgs = append(ourArgs, nodeArgs...)
	out, err := exec.Command("node", ourArgs...).Output()
	if err != nil {
		zap.L().Error("error running node", zap.Error(err), zap.ByteString("output", out))
	}
	return string(out), nil
}

func (p *PSMLoadApp) Work(board string) error {
	p.boardId = board
	wp := workerpool.New(int(p.Workers))

	for i := int64(0); i < p.Workers; i++ {
		r := i
		wp.Submit(func() {
			for {
				p.work(r)
			}
		})
	}

	wp.StopWait()
	return nil
}
func (p *PSMLoadApp) getOffer(want float64) (string, error) {
	resp, err := p.invokeNode("--wantStable", fmt.Sprintf("%f", want), "--boardId", p.boardId, "--feePct", fmt.Sprintf("%f", p.Fee))
	if err != nil {
		return "", err
	}
	return resp, nil
}

func (p *PSMLoadApp) broadcastOffer(idx int64, offerData string) (string, error) {
	//agd broadcast
	cmdargs := []string{"--node", p.instagoricToService("rpc"),
		"--chain-id", p.chainId, "--from", keyName(idx), "--keyring-backend=test", "-b", "sync", "--home", p.homeDir, "tx", "swingset", "wallet-action", "-y", "--allow-spend", offerData}
	cmd := exec.Command("agd", cmdargs...)
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	err := cmd.Run()

	if err != nil {
		if strings.Index(errb.String(), "not found: key not found") >= 0 {
			p.provisionAddress(p.keys[idx])
			time.Sleep(10 * time.Second)
			zap.L().Info("client is not provisioned", zap.String("addr", p.keys[idx]))
		} else {
			zap.L().Error("error broadcasting offer", zap.Error(err), zap.String("output", outb.String()), zap.String("stderr", errb.String()))
		}
		//try reprovisioning
		return "", fmt.Errorf("error broadcasting offer")
	}

	for _, match := range txChain.FindAllStringSubmatch(string(outb.String()), -1) {

		return match[1], nil
	}
	return "", nil
}

func (p *PSMLoadApp) getTx(tx string) (*TxResponse, error) {
	endpoint := p.instagoricToService("api")
	resp, err := p.client.Get(fmt.Sprintf("%s/cosmos/tx/v1beta1/txs/%s", endpoint, tx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 404:
		return nil, fmt.Errorf("not found yet")
	case 200:
		var rp TxResponse
		dec := json.NewDecoder(resp.Body)
		err = dec.Decode(&rp)
		if err != nil {
			return nil, err
		}
		return &rp, nil
	default:
		return nil, fmt.Errorf("unknown status %d", resp.StatusCode)

	}
}
func (p *PSMLoadApp) waitForTx(tx string) error {
	//agd query height
	for i := 0; i < 50; i++ {
		txStatus, err := p.getTx(tx)
		if err != nil {
			zap.L().Debug("error getting tx", zap.String("tx", tx), zap.Error(err))
			time.Sleep(500 * time.Millisecond)
			continue
		}
		switch txStatus.TxResponse.Code {
		case 0:
			return nil
		default:
			zap.L().Warn("tx was invalid", zap.String("tx", tx), zap.String("raw_log", txStatus.TxResponse.RawLog), zap.Int("code", txStatus.TxResponse.Code), zap.Error(err))
			return fmt.Errorf("invalid tx")
		}
	}

	return nil
}
func (p *PSMLoadApp) work(idx int64) {
	start := time.Now()
	offer, err := p.getOffer(0.01)
	if err != nil {
		zap.L().Error("error on cycle", zap.Error(err), zap.Int64("duration_ms", time.Now().Sub(start).Milliseconds()), zap.Int64("duration_nano", time.Now().Sub(start).Nanoseconds()))
		return
	}

	txhash, err := p.broadcastOffer(idx, offer)
	if err != nil {
		//zap.L().Error("error on cycle", zap.Error(err), zap.Int64("duration_ms", time.Now().Sub(start).Milliseconds()), zap.Int64("duration_nano", time.Now().Sub(start).Nanoseconds()))
		return
	}

	// broadcast offer
	err = p.waitForTx(txhash)
	if err != nil {
		zap.L().Error("error waiting for tx", zap.Error(err), zap.Int64("duration_ms", time.Now().Sub(start).Milliseconds()), zap.Int64("duration_nano", time.Now().Sub(start).Nanoseconds()))
		return
	}

	zap.L().Info("completed cycle", zap.Int64("idx", idx), zap.String("address", keyName(idx)), zap.Int64("duration_ms", time.Now().Sub(start).Milliseconds()), zap.Int64("duration_nano", time.Now().Sub(start).Nanoseconds()))
}

func (p *PSMLoadApp) GetBoard() (string, error) {
	resp, err := p.invokeNode("--contract")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(resp, "board") {
		return strings.TrimSpace(resp), nil
	}
	return "", fmt.Errorf("error getting contract id")
}
func (p *PSMLoadApp) Cleanup() {
	os.RemoveAll(p.homeDir)
}
