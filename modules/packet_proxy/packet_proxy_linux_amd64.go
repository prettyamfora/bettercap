package packet_proxy

import (
	"fmt"
	"io/ioutil"
	golog "log"
	"plugin"
	"strings"
	"syscall"

	"github.com/bettercap/bettercap/core"
	"github.com/bettercap/bettercap/session"

	"github.com/chifflier/nfqueue-go/nfqueue"

	"github.com/evilsocket/islazy/fs"
	"github.com/evilsocket/islazy/tui"
)

type PacketProxy struct {
	session.SessionModule
	done       chan bool
	chainName  string
	rule       string
	queue      *nfqueue.Queue
	queueNum   int
	queueCb    nfqueue.Callback
	pluginPath string
	plugin     *plugin.Plugin
}

// this is ugly, but since we can only pass a function
// (not a struct function) as a callback to nfqueue,
// we need this in order to recover the state.
var mod *PacketProxy

func NewPacketProxy(s *session.Session) *PacketProxy {
	mod = &PacketProxy{
		SessionModule: session.NewSessionModule("packet.proxy", s),
		done:          make(chan bool),
		queue:         nil,
		queueCb:       nil,
		queueNum:      0,
		chainName:     "OUTPUT",
	}

	mod.AddHandler(session.NewModuleHandler("packet.proxy on", "",
		"Start the NFQUEUE based packet proxy.",
		func(args []string) error {
			return mod.Start()
		}))

	mod.AddHandler(session.NewModuleHandler("packet.proxy off", "",
		"Stop the NFQUEUE based packet proxy.",
		func(args []string) error {
			return mod.Stop()
		}))

	mod.AddParam(session.NewIntParameter("packet.proxy.queue.num",
		"0",
		"NFQUEUE number to bind to."))

	mod.AddParam(session.NewStringParameter("packet.proxy.chain",
		"OUTPUT",
		"",
		"Chain name of the iptables rule."))

	mod.AddParam(session.NewStringParameter("packet.proxy.plugin",
		"",
		"",
		"Go plugin file to load and call for every packet."))

	mod.AddParam(session.NewStringParameter("packet.proxy.rule",
		"",
		"",
		"Any additional iptables rule to make the queue more selective (ex. --destination 8.8.8.8)."))

	return mod
}

func (pp PacketProxy) Name() string {
	return "packet.proxy"
}

func (pp PacketProxy) Description() string {
	return "A Linux only module that relies on NFQUEUEs in order to filter packets."
}

func (pp PacketProxy) Author() string {
	return "Simone Margaritelli <evilsocket@gmail.com>"
}

func (pp *PacketProxy) destroyQueue() {
	if pp.queue == nil {
		return
	}

	pp.queue.DestroyQueue()
	pp.queue.Close()
	pp.queue = nil
}

func (pp *PacketProxy) runRule(enable bool) (err error) {
	action := "-A"
	if !enable {
		action = "-D"
	}

	args := []string{
		action, pp.chainName,
	}

	if pp.rule != "" {
		rule := strings.Split(pp.rule, " ")
		args = append(args, rule...)
	}

	args = append(args, []string{
		"-j", "NFQUEUE",
		"--queue-num", fmt.Sprintf("%d", pp.queueNum),
		"--queue-bypass",
	}...)

	pp.Debug("iptables %s", args)

	_, err = core.Exec("iptables", args)
	return
}

func (pp *PacketProxy) Configure() (err error) {
	golog.SetOutput(ioutil.Discard)

	pp.destroyQueue()

	if err, pp.queueNum = pp.IntParam("packet.proxy.queue.num"); err != nil {
		return
	} else if err, pp.chainName = pp.StringParam("packet.proxy.chain"); err != nil {
		return
	} else if err, pp.rule = pp.StringParam("packet.proxy.rule"); err != nil {
		return
	} else if err, pp.pluginPath = pp.StringParam("packet.proxy.plugin"); err != nil {
		return
	}

	if pp.pluginPath == "" {
		return fmt.Errorf("The parameter %s can not be empty.", tui.Bold("packet.proxy.plugin"))
	} else if !fs.Exists(pp.pluginPath) {
		return fmt.Errorf("%s does not exist.", pp.pluginPath)
	}

	pp.Info("loading packet proxy plugin from %s ...", pp.pluginPath)

	var ok bool
	var sym plugin.Symbol

	if pp.plugin, err = plugin.Open(pp.pluginPath); err != nil {
		return
	} else if sym, err = pp.plugin.Lookup("OnPacket"); err != nil {
		return
	} else if pp.queueCb, ok = sym.(func(*nfqueue.Payload) int); !ok {
		return fmt.Errorf("Symbol OnPacket is not a valid callback function.")
	}

	pp.queue = new(nfqueue.Queue)
	if err = pp.queue.SetCallback(dummyCallback); err != nil {
		return
	} else if err = pp.queue.Init(); err != nil {
		return
	} else if err = pp.queue.Unbind(syscall.AF_INET); err != nil {
		return
	} else if err = pp.queue.Bind(syscall.AF_INET); err != nil {
		return
	} else if err = pp.queue.CreateQueue(pp.queueNum); err != nil {
		return
	} else if err = pp.queue.SetMode(nfqueue.NFQNL_COPY_PACKET); err != nil {
		return
	} else if err = pp.runRule(true); err != nil {
		return
	}

	return nil
}

// we need this because for some reason we can't directly
// pass the symbol loaded from the plugin as a direct
// CGO callback ... ¯\_(ツ)_/¯
func dummyCallback(payload *nfqueue.Payload) int {
	return mod.queueCb(payload)
}

func (pp *PacketProxy) Start() error {
	if pp.Running() {
		return session.ErrAlreadyStarted
	} else if err := pp.Configure(); err != nil {
		return err
	}

	return pp.SetRunning(true, func() {
		pp.Info("started on queue number %d", pp.queueNum)

		defer pp.destroyQueue()

		pp.queue.Loop()

		pp.done <- true
	})
}

func (pp *PacketProxy) Stop() error {
	return pp.SetRunning(false, func() {
		pp.queue.StopLoop()
		pp.runRule(false)
		<-pp.done
	})
}