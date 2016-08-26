// 研究了一天的状态机，天气也热，正当我写的即将昏迷之际，我突然醒悟了，原来状态机是这么一回事
// - 状态比喻成 数据结构
// - 事件比喻成 用户输入
// - 状态转移则是函数调用
// 如此依赖写成函数，也就是 (Orz 原来如此)
// type FSM struct {
// 	State 			FSMState
// 	TransformFuncs  map[FSMState] func()
// }

// func (f *FSM) UserAction(action FSMAction) {
// 	...
// }

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/codeskyblue/kexec"
)

type FSMState string
type FSMEvent string
type FSMHandler func()

type FSM struct {
	mu       sync.Mutex
	state    FSMState
	handlers map[FSMState]map[FSMEvent]FSMHandler

	StateChange func(oldState, newState FSMState)
}

func (f *FSM) AddHandler(state FSMState, event FSMEvent, hdlr FSMHandler) *FSM {
	_, ok := f.handlers[state]
	if !ok {
		f.handlers[state] = make(map[FSMEvent]FSMHandler)
	}
	if _, ok = f.handlers[state][event]; ok {
		log.Fatalf("set twice for state(%s) event(%s)", state, event)
	}
	f.handlers[state][event] = hdlr
	return f
}

func (f *FSM) State() FSMState {
	return f.state
}

func (f *FSM) SetState(newState FSMState) {
	if f.StateChange != nil {
		f.StateChange(f.state, newState)
	}
	f.state = newState
}

func (f *FSM) Operate(event FSMEvent) FSMState {
	f.mu.Lock()
	defer f.mu.Unlock()

	eventMap := f.handlers[f.State()]
	if eventMap == nil {
		return f.State()
	}
	if fn, ok := eventMap[event]; ok {
		fn()
	}
	return f.State()
}

func NewFSM(initState FSMState) *FSM {
	return &FSM{
		state:    initState,
		handlers: make(map[FSMState]map[FSMEvent]FSMHandler),
	}
}

// Only 4 states now is enough, I think
var (
	Running   = FSMState("running")
	Stopped   = FSMState("stopped")
	Fatal     = FSMState("fatal")
	RetryWait = FSMState("retry wait")

	StartEvent   = FSMEvent("start")
	StopEvent    = FSMEvent("stop")
	RestartEvent = FSMEvent("restart")
)

type Program struct {
	Name         string   `yaml:"name" json:"name"`
	Command      string   `yaml:"command" json:"command"`
	Environ      []string `yaml:"environ" json:"environ"`
	Dir          string   `yaml:"directory" json:"directory"`
	AutoStart    bool     `yaml:"autostart" json:"autostart"` // change to *bool, which support unexpected
	StartRetries int      `yaml:"startretries" json:"startretries"`
	StartSeconds int      `yaml:"startsecs" json:"startsecs"`
	// LogDir       string   `yaml:"logdir"`
}

func (p *Program) Check() error {
	if p.Name == "" {
		return errors.New("Program name empty")
	}
	if p.Command == "" {
		return errors.New("Program command empty")
	}
	if p.Dir != "" && !IsDir(p.Dir) {
		return fmt.Errorf("Program dir(%s) not exists", p.Dir)
	}
	return nil
}

type Process struct {
	*FSM      `json:"-"`
	Program   `json:"program"`
	cmd       *kexec.KCommand
	stopC     chan syscall.Signal
	retryLeft int
	Status    string `json:"status"`
}

func (p *Process) buildCommand() *kexec.KCommand {
	cmd := kexec.CommandString(p.Command) // Not tested here, I think it should work
	// cmd := kexec.Command(p.Command[0], p.Command[1:]...)
	cmd.Dir = p.Dir
	cmd.Env = append(os.Environ(), p.Environ...)
	return cmd
}

func (p *Process) waitNextRetry() {
	p.SetState(RetryWait)
	if p.retryLeft <= 0 {
		p.retryLeft = p.StartRetries
		p.SetState(Fatal)
		return
	}
	p.retryLeft -= 1
	select {
	case <-time.After(2 * time.Second): // TODO: need put it into Program
		p.startCommand()
	case <-p.stopC:
		p.stopCommand()
	}
}

func (p *Process) stopCommand() {
	if p.cmd == nil {
		return
	}
	p.cmd.Terminate(syscall.SIGKILL)
	p.cmd = nil
	time.Sleep(200 * time.Millisecond)
	p.SetState(Stopped)
}

func (p *Process) IsRunning() bool {
	return p.State() == Running || p.State() == RetryWait
}

func (p *Process) startCommand() {
	p.stopCommand()
	log.Println("start cmd:", p.Name, p.Command)
	p.cmd = kexec.CommandString(p.Command)
	p.cmd.Stdout = os.Stdout

	p.SetState(Running)
	go func() {
		errC := GoFunc(p.cmd.Run)
		startTime := time.Now()
		select {
		case err := <-errC: //<-GoTimeoutFunc(time.Duration(p.StartSeconds)*time.Second, p.cmd.Run):
			log.Println(err, time.Since(startTime))
			if time.Since(startTime) < time.Duration(p.StartSeconds)*time.Second {
				if p.retryLeft == p.StartRetries { // If first time quit so fast, just set to fatal
					p.SetState(Fatal)
					log.Println("Start change to fatal")
					return
				}
			}
			p.waitNextRetry()
		case <-p.stopC:
			log.Println("recv stop command")
			p.stopCommand()
		}
	}()
}

func NewProcess(pg Program) *Process {
	pr := &Process{
		FSM:       NewFSM(Stopped),
		Program:   pg,
		stopC:     make(chan syscall.Signal),
		retryLeft: pg.StartRetries,
		Status:    string(Stopped),
	}
	pr.StateChange = func(_, newStatus FSMState) {
		pr.Status = string(newStatus)
	}
	if pr.StartSeconds <= 0 {
		pr.StartSeconds = 3
	}

	pr.AddHandler(Stopped, StartEvent, func() {
		pr.retryLeft = pr.StartRetries
		pr.startCommand()
	})
	pr.AddHandler(Fatal, StartEvent, pr.startCommand)

	pr.AddHandler(Running, StopEvent, func() {
		select {
		case pr.stopC <- syscall.SIGTERM:
		case <-time.After(200 * time.Millisecond):
		}
	}).AddHandler(Running, RestartEvent, func() {
		go func() {
			pr.Operate(StopEvent)
			// TODO: start laterly
			time.Sleep(1 * time.Second)
			pr.Operate(StartEvent)
		}()
	})
	return pr
}

func init() {
	// pg := Program{
	// 	Name:    "demo",
	// 	Command: "echo hello world && sleep 1 && echo end",
	// }
	// proc := NewProcess(pg)
	// log.Println(proc.State())

	// proc.Operate(StartEvent)
	// log.Println(proc.State())
	// time.Sleep(2 * time.Second)
	// log.Println(proc.State())
	// proc.Operate(StopEvent)
	// time.Sleep(1 * time.Second)
	// log.Println(proc.State())
	// log.Println(light.State)
	// light.AddHandler(Opened, Close, func() {
	// 	log.Println("Close light")
	// 	light.State = Closed
	// })
	// light.Operate(Close)
	// log.Println(light.State)
}
