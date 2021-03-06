package dkron

import (
	"math/rand"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/armon/circbuf"
	"github.com/hashicorp/serf/serf"
)

const (
	windows = "windows"

	// maxBufSize limits how much data we collect from a handler.
	// This is to prevent Serf's memory from growing to an enormous
	// amount due to a faulty handler.
	maxBufSize = 256000
)

// spawn command that specified as proc.
func spawnProc(proc string) (*exec.Cmd, error) {
	cs := []string{"/bin/bash", "-c", proc}
	cmd := exec.Command(cs[0], cs[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ())

	log.WithFields(logrus.Fields{
		"proc": proc,
	}).Info("proc: Starting")

	err := cmd.Start()
	if err != nil {
		log.Errorf("proc: Failed to start %s: %s\n", proc, err)
		return nil, err
	}
	return cmd, nil
}

// invokeJob will execute the given job. Depending on the event.
func (a *AgentCommand) invokeJob(execution *Execution) error {
	job := execution.Job

	output, _ := circbuf.NewBuffer(maxBufSize)

	// Determine the shell invocation based on OS
	var shell, flag string
	if runtime.GOOS == windows {
		shell = "cmd"
		flag = "/C"
	} else {
		shell = "/bin/sh"
		flag = "-c"
	}

	cmd := exec.Command(shell, flag, job.Command)
	cmd.Stderr = output
	cmd.Stdout = output

	// Start a timer to warn about slow handlers
	slowTimer := time.AfterFunc(2*time.Hour, func() {
		log.Warnf("proc: Script '%s' slow, execution exceeding %v", job.Command, 2*time.Hour)
	})

	if err := cmd.Start(); err != nil {
		return err
	}

	// Warn if buffer is overritten
	if output.TotalWritten() > output.Size() {
		log.Warnf("proc: Script '%s' generated %d bytes of output, truncated to %d", job.Command, output.TotalWritten(), output.Size())
	}

	var success bool
	err := cmd.Wait()
	slowTimer.Stop()
	log.WithFields(logrus.Fields{
		"output": output,
	}).Debug("proc: Command output")
	if err != nil {
		log.WithFields(logrus.Fields{
			"err": err,
		}).Error("proc: command error output")
		success = false
	} else {
		success = true
	}

	execution.FinishedAt = time.Now()
	execution.Success = success
	execution.Output = output.Bytes()

	rpcServer, err := a.queryRPCConfig()
	if err != nil {
		return err
	}

	rc := &RPCClient{ServerAddr: string(rpcServer)}
	return rc.callExecutionDone(execution)
}

func (a *AgentCommand) selectServer() serf.Member {
	servers := a.listServers()
	server := servers[rand.Intn(len(servers))]

	return server
}

func (a *AgentCommand) queryRPCConfig() ([]byte, error) {
	nodeName := a.selectServer().Name

	params := &serf.QueryParam{
		FilterNodes: []string{nodeName},
		FilterTags:  map[string]string{"server": "true"},
		RequestAck:  true,
	}

	qr, err := a.serf.Query(QueryRPCConfig, nil, params)
	if err != nil {
		log.WithFields(logrus.Fields{
			"query": QueryRPCConfig,
			"error": err,
		}).Fatal("proc: Error sending query")
		return nil, err
	}
	defer qr.Close()

	ackCh := qr.AckCh()
	respCh := qr.ResponseCh()

	var rpcAddr []byte
	for !qr.Finished() {
		select {
		case ack, ok := <-ackCh:
			if ok {
				log.WithFields(logrus.Fields{
					"query": QueryRPCConfig,
					"from":  ack,
				}).Debug("proc: Received ack")
			}
		case resp, ok := <-respCh:
			if ok {
				log.WithFields(logrus.Fields{
					"query":   QueryRPCConfig,
					"from":    resp.From,
					"payload": string(resp.Payload),
				}).Debug("proc: Received response")

				rpcAddr = resp.Payload
			}
		}
	}
	log.WithFields(logrus.Fields{
		"query": QueryRPCConfig,
	}).Debug("proc: Done receiving acks and responses")

	return rpcAddr, nil
}
