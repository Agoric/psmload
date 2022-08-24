package main

import (
	"flag"
	"github.com/agoric/psmload/psmload"
	"go.uber.org/zap"
	"os"
)

func main() {
	os.Setenv("NODE_OPTIONS", "--experimental-fetch")
	os.Setenv("NODE_NO_WARNINGS", "1")

	logger, _ := zap.NewProduction()
	defer logger.Sync() // flushes buffer, if any
	zap.ReplaceGlobals(logger)

	workers := flag.Int64("workers", 10, "number of smart wallet workers")
	instagoric := flag.String("instagoric", "https://ollinet.agoric.net:443", "instagoric root, e.g. https://ollinet.agoric.net:443")
	fee := flag.Float64("fee", 0.011, "fee to pay, eg. 0.011")
	stateDir := flag.String("statedir", "", "directory to keep state in/resume from")
	flag.Parse()
	client := psmload.NewPSMLoadApp(*instagoric, *workers, *fee, *stateDir)
	//defer client.Cleanup()
	client.CreateKeys()
	board, err := client.GetBoard()
	if err != nil {
		panic(err)
	}
	//client.ProvisionKeys()
	client.Work(board)

}
