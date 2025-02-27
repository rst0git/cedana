package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"

	"github.com/checkpoint-restore/go-criu/v6/rpc"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// Code for interfacing with CRIU. We could use go-criu, but there are certain limitations in the abstractions
// presented. Most of the code found here is lifted from https://github.com/checkpoint-restore/go-criu/blob/master/main.go.
type Criu struct {
}

func (c *Criu) sendAndRecv(reqB []byte, sk *os.File) ([]byte, int, error) {
	_, err := sk.Write(reqB)
	if err != nil {
		return nil, 0, err
	}

	respB := make([]byte, 2*4096)
	n, err := sk.Read(respB)
	if err != nil {
		return nil, 0, err
	}

	return respB, n, nil
}

func (c *Criu) doSwrk(reqType rpc.CriuReqType, opts *rpc.CriuOpts, nfy *Notify, extraFiles []*os.File) (*rpc.CriuResp, error) {
	resp, err := c.doSwrkWithResp(reqType, opts, nfy, extraFiles, nil)
	if err != nil {
		return nil, err
	}
	respType := resp.GetType()
	if respType != reqType {
		return nil, errors.New("unexpected CRIU RPC response")
	}

	return resp, nil
}

// WriteJSON writes the provided struct v to w using standard json marshaling
// without a trailing newline. This is used instead of json.Encoder because
// there might be a problem in json decoder in some cases, see:
// https://github.com/docker/docker/issues/14203#issuecomment-174177790
func WriteJSON(w io.Writer, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// SendRawFd sends a specific file descriptor over the given AF_UNIX socket.
func SendRawFd(socket *os.File, msg string, fd uintptr) error {
	oob := unix.UnixRights(int(fd))
	return unix.Sendmsg(int(socket.Fd()), []byte(msg), oob, nil, 0)
}

const MaxNameLen = 4096

// SendFile sends a file over the given AF_UNIX socket. file.Name() is also
// included so that if the other end uses RecvFile, the file will have the same
// name information.
func SendFile(socket *os.File, file *os.File) error {
	name := file.Name()
	if len(name) >= MaxNameLen {
		return fmt.Errorf("sendfd: filename too long: %s", name)
	}
	err := SendRawFd(socket, name, file.Fd())
	runtime.KeepAlive(file)
	return err
}

func (c *Criu) doSwrkWithResp(reqType rpc.CriuReqType, opts *rpc.CriuOpts, nfy *Notify, extraFiles []*os.File, features *rpc.CriuFeatures) (*rpc.CriuResp, error) {
	var resp *rpc.CriuResp

	req := rpc.CriuReq{
		Type: &reqType,
		Opts: opts,
	}

	if nfy != nil {
		opts.NotifyScripts = proto.Bool(true)
	}

	if features != nil {
		req.Features = features
	}

	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		return nil, err
	}

	cln := os.NewFile(uintptr(fds[0]), "criu-xprt-cln")
	syscall.CloseOnExec(fds[0])

	srv := os.NewFile(uintptr(fds[1]), "criu-xprt-srv")
	defer srv.Close()

	cmd := exec.Command("criu", "swrk", strconv.Itoa(fds[1]))

	if extraFiles != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, extraFiles...)
	}

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	for {
		reqB, err := proto.Marshal(&req)
		if err != nil {
			return nil, err
		}

		respB, respS, err := c.sendAndRecv(reqB, cln)
		if err != nil {
			return nil, err
		}

		resp = &rpc.CriuResp{}
		err = proto.Unmarshal(respB[:respS], resp)
		if err != nil {
			return nil, err
		}

		if !resp.GetSuccess() {
			return resp, fmt.Errorf("operation failed (msg:%s err:%d)",
				resp.GetCrErrmsg(), resp.GetCrErrno())
		}

		respType := resp.GetType()
		if respType != rpc.CriuReqType_NOTIFY {
			break
		}
		if nfy == nil {
			return resp, errors.New("unexpected notify")
		}

		notify := resp.GetNotify()
		switch notify.GetScript() {
		case "pre-dump":
			err = nfy.PreDump()
		case "post-dump":
			err = nfy.PostDump()
		case "pre-restore":
			err = nfy.PreRestore()
		case "post-restore":
			err = nfy.PostRestore(notify.GetPid())
		case "network-lock":
			err = nfy.NetworkLock()
		case "network-unlock":
			err = nfy.NetworkUnlock()
		case "setup-namespaces":
			err = nfy.SetupNamespaces(notify.GetPid())
		case "post-setup-namespaces":
			err = nfy.PostSetupNamespaces()
		case "post-resume":
			err = nfy.PostResume()
		case "pre-resume":
			err = nfy.PreResume()
		default:
			err = nil
		}

		if err != nil {
			return resp, err
		}

		req = rpc.CriuReq{
			Type:          &respType,
			NotifySuccess: proto.Bool(true),
		}
	}

	// cleanup
	err = cmd.Wait()
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Dump dumps a process
func (c *Criu) Dump(opts *rpc.CriuOpts, nfy *Notify) (*rpc.CriuResp, error) {
	return c.doSwrk(rpc.CriuReqType_DUMP, opts, nfy, nil)
}

// Restore restores a process
func (c *Criu) Restore(opts *rpc.CriuOpts, nfy *Notify, extraFiles []*os.File) (*rpc.CriuResp, error) {
	return c.doSwrk(rpc.CriuReqType_RESTORE, opts, nfy, extraFiles)
}

func (c *Criu) GetCriuVersion() (int, error) {
	resp, err := c.doSwrkWithResp(rpc.CriuReqType_VERSION, nil, nil, nil, nil)
	if err != nil {
		return 0, err
	}

	if resp.GetType() != rpc.CriuReqType_VERSION {
		return 0, fmt.Errorf("unexpected CRIU RPC response")
	}

	version := int(*resp.GetVersion().MajorNumber) * 10000
	version += int(*resp.GetVersion().MinorNumber) * 100
	if resp.GetVersion().Sublevel != nil {
		version += int(*resp.GetVersion().Sublevel)
	}

	if resp.GetVersion().Gitid != nil {
		// taken from runc: if it is a git release -> increase minor by 1
		version -= (version % 100)
		version += 100
	}

	return version, nil
}

// IsCriuAtLeast checks if the version is at least the same
// as the parameter version
func (c *Criu) IsCriuAtLeast(version int) (bool, error) {
	criuVersion, err := c.GetCriuVersion()
	if err != nil {
		return false, err
	}

	if criuVersion >= version {
		return true, nil
	}

	return false, nil
}
