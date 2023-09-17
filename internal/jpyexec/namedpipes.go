package jpyexec

// This file implements the polling on the $GONB_PIPE and $GONB_PIPE_WIDGETS named pipes created
// to receive information from the program being executed and to send information from the
// widgets.
//
// It has a protocol (defined under `gonbui/protocol`) to display rich content.

import (
	"encoding/gob"
	"fmt"
	"github.com/janpfeifer/gonb/gonbui/protocol"
	"github.com/janpfeifer/gonb/kernel"
	"github.com/pkg/errors"
	"io"
	"k8s.io/klog/v2"
	"os"
	"sync"
	"syscall"
)

func init() {
	// Register generic gob types we want to make sure are understood.
	gob.Register(map[string]any{})
	gob.Register([]string{})
	gob.Register([]any{})
}

// handleNamedPipes creates the named pipe and set up the goroutines to listen to them.
//
// TODO: make this more secure, maybe with a secret key also passed by the environment.
func (exec *Executor) handleNamedPipes() (err error) {
	// Create temporary named pipes in both directions.
	exec.namedPipeReaderPath, err = exec.createTmpFifo()
	if err != nil {
		return err
	}
	exec.namedPipeWriterPath, err = exec.createTmpFifo()
	if err != nil {
		return err
	}
	exec.cmd.Env = append(exec.cmd.Environ(),
		protocol.GONB_PIPE_ENV+"="+exec.namedPipeReaderPath,
		protocol.GONB_PIPE_BACK_ENV+"="+exec.namedPipeWriterPath)

	exec.openPipeReader()
	return
}

func (exec *Executor) createTmpFifo() (string, error) {
	// Create a temporary file name.
	f, err := os.CreateTemp(exec.dir, "gonb_pipe_")
	if err != nil {
		return "", err
	}
	pipePath := f.Name()
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Remove(pipePath); err != nil {
		return "", err
	}

	// Create pipe.
	if err = syscall.Mkfifo(pipePath, 0600); err != nil {
		return "", errors.Wrapf(err, "failed to create pipe (Mkfifo) for %q", pipePath)
	}
	return pipePath, nil
}

// openPipeReader opens `exec.namedPipeReaderPath` and handles its proper closing, and removal of
// the named pipe when program execution is finished.
//
// The doneChan is listened to: when it is closed, it will trigger the listener goroutine to close the pipe,
// remove it and quit.
func (exec *Executor) openPipeReader() {
	// Synchronize pipe: if it's not opened by the program being executed,
	// we have to open it ourselves for writing, to avoid blocking
	// `os.Open` (it waits the other end of the fifo to be opened before returning).
	// See discussion in:
	// https://stackoverflow.com/questions/75255426/how-to-interrupt-a-blocking-os-open-call-waiting-on-a-fifo-in-go
	var muFifo sync.Mutex
	fifoOpenedForReading := false

	go func() {
		// Clean up after program is over, there are two scenarios:
		// 1. The executed program opened the pipe: then we just remove the pipePath.
		// 2. The executed program never opened the pipe: then the other end (goroutine
		//    below) will be forever blocked on os.Open call.
		<-exec.doneChan
		muFifo.Lock()
		if !fifoOpenedForReading {
			w, err := os.OpenFile(exec.namedPipeReaderPath, os.O_WRONLY, 0600)
			if err == nil {
				// Closing it allows the open of the pipe for reading (below) to unblock.
				_ = w.Close()
			}
		}
		muFifo.Unlock()
		_ = os.Remove(exec.namedPipeReaderPath)
	}()

	go func() {
		if exec.isDone {
			// In case program execution interrupted early.
			return
		}
		// Notice that opening pipeReader below blocks, until the other end
		// (the go program being executed) opens it as well.
		var err error
		exec.pipeReader, err = os.Open(exec.namedPipeReaderPath)
		if err != nil {
			klog.Warningf("Failed to open pipe (Mkfifo) %q for reading: %+v", exec.namedPipeReaderPath, err)
			return
		}
		muFifo.Lock()
		fifoOpenedForReading = true
		defer muFifo.Unlock()

		// Start polling of the pipeReader.
		go exec.pollNamedPipeReader()

		// Wait program execution to finish to close reader (in case it is not yet closed).
		<-exec.doneChan
		_ = exec.pipeReader.Close()
		_ = os.Remove(exec.namedPipeReaderPath)
	}()
}

// pollNamedPipeReader will continuously read for incoming requests with displaying content
// on the notebook or widgets updates.
func (exec *Executor) pollNamedPipeReader() {
	decoder := gob.NewDecoder(exec.pipeReader)
	for {
		data := &protocol.DisplayData{}
		err := decoder.Decode(data)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) {
			return
		} else if err != nil {
			klog.Infof("Named pipe: failed to parse message: %+v", err)
			return
		}

		// Special case for a request for input:
		if reqAny, found := data.Data[protocol.MIMEJupyterInput]; found {
			klog.V(2).Infof("Received InputRequest: %v", reqAny)
			req, ok := reqAny.(*protocol.InputRequest)
			if !ok {
				exec.reportCellError(errors.New("A MIMEJupyterInput sent to GONB_PIPE without an associated protocol.InputRequest!?"))
				continue
			}
			exec.dispatchInputRequest(req)
			continue
		}

		// Otherwise, just display with the corresponding MIME type:
		exec.dispatchDisplayData(data)
	}
}

// reportCellError reports error to both, the notebook and the standard logger (gonb's stderr).
func (exec *Executor) reportCellError(err error) {
	errStr := fmt.Sprintf("%+v", err) // Error with stack.
	klog.Errorf("%s", errStr)
	err = kernel.PublishWriteStream(exec.msg, kernel.StreamStderr, errStr)
	if err != nil {
		klog.Errorf("%+v", errors.WithStack(err))
	}
}

// dispatchDisplayData received through the named pipe (`$GONB_PIPE`).
func (exec *Executor) dispatchDisplayData(data *protocol.DisplayData) {
	// Log info about what is being displayed.
	msgData := kernel.Data{
		Data:      make(kernel.MIMEMap, len(data.Data)),
		Metadata:  make(kernel.MIMEMap),
		Transient: make(kernel.MIMEMap),
	}
	for mimeType, content := range data.Data {
		msgData.Data[string(mimeType)] = content
	}
	if klog.V(1).Enabled() {
		kernel.LogDisplayData(msgData.Data)
	}
	for key, content := range data.Metadata {
		msgData.Metadata[key] = content
	}
	var err error
	if data.DisplayID != "" {
		msgData.Transient["display_id"] = data.DisplayID
		err = kernel.PublishUpdateDisplayData(exec.msg, msgData)
	} else {
		err = kernel.PublishData(exec.msg, msgData)
	}
	if err != nil {
		klog.Errorf("Failed to display data (ignoring): %v", err)
	}
}

// dispatchInputRequest uses the standard Jupyter input mechanism.
// It is fundamentally broken -- it locks the UI even if the program already stopped running --
// so we suggest using the `gonb/gonbui/widgets` API instead.
func (exec *Executor) dispatchInputRequest(req *protocol.InputRequest) {
	klog.V(2).Infof("Received InputRequest %+v", req)
	writeStdinFn := func(original, input *kernel.MessageImpl) error {
		content := input.Composed.Content.(map[string]any)
		value := content["value"].(string) + "\n"
		klog.V(2).Infof("stdin value: %q", value)
		go func() {
			exec.muDone.Lock()
			cmdStdin := exec.cmdStdin
			exec.muDone.Unlock()
			if exec.isDone {
				return
			}
			// Write concurrently, not to block, in case program doesn't
			// actually read anything from the stdin.
			_, err := cmdStdin.Write([]byte(value))
			if err != nil {
				// Could happen if something was not fully written, and channel was closed, in
				// which case it's ok.
				klog.Warningf("failed to write to stdin of cell: %+v", err)
			}
		}()
		return nil
	}
	err := exec.msg.PromptInput(req.Prompt, req.Password, writeStdinFn)
	if err != nil {
		exec.reportCellError(err)
	}
}
