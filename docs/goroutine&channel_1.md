# Go 并发编程深度指南：Goroutine 与 Channel 原理全解析

> 说明：本文基于 Go 1.14+ 的运行时模型，并参考当前官方源码中的 `runtime.g`、`runtime.m`、`runtime.p`、`runtime.hchan`、`runtime.sudog`、`selectgo` 等结构。Runtime 内部字段会随版本变化，但核心模型稳定。

---

# 前言：了解runtime
## 1. runtime 是什么

Go 程序不是直接从你的 main.main() 开始跑的。每个 Go 二进制都会打进一套 runtime，它负责：

创建并调度 goroutine
管理 goroutine 栈
堆内存分配
垃圾回收 GC
channel、mutex、timer、netpoll 的底层协作
panic/defer/recover
map、interface、reflect 所需的类型元数据
syscall/cgo/race detector/profiler/trace 支持
你写：

go f()
看起来只是语言语法，底层会变成 runtime 创建一个 G，把它放进调度队列，然后由调度器找线程执行。

## 2. runtime 包的常用 API

常见用法：

runtime.NumCPU()
runtime.NumGoroutine()
runtime.GOMAXPROCS(0)
runtime.Gosched()
runtime.GC()
runtime.ReadMemStats(&m)
runtime.Caller(0)
runtime.Callers(...)
runtime.LockOSThread()
runtime.KeepAlive(x)
runtime.SetFinalizer(x, f)
但很多 API 不该滥用：

runtime.GC()：一般不要手动调，除非测试、基准、特殊释放场景。
runtime.Gosched()：让出当前 P 给别的 goroutine，用得很少。
SetFinalizer：不要当析构函数用，执行时间不确定。
LockOSThread：只在 GUI、OpenGL、某些 syscall/cgo 需要固定 OS 线程时用。
KeepAlive：防止对象被编译器/GC 过早认为不可达，常见于 fd、unsafe、syscall 场景。
运行时环境变量也很重要：

GOMAXPROCS=8
GOGC=100
GOMEMLIMIT=2GiB
GODEBUG=gctrace=1,schedtrace=1000
GOTRACEBACK=all
## 3. 启动流程

粗略流程是：

OS 启动进程
-> runtime 的汇编入口 rt0
-> 初始化 TLS / g0 / m0
-> osinit
-> schedinit
-> mallocinit
-> gcinit
-> 创建 main goroutine
-> 执行所有 package init
-> 调用 main.main
所以 main.main() 之前，调度器、内存分配器、GC、系统监控线程基本都已经准备好了。

## 4. GMP 调度模型

Go 调度器核心是 GMP：

G = goroutine，用户态执行单元
M = machine，OS 线程
P = processor，执行 Go 代码所需的逻辑处理器
关系：

M 必须拿到 P 才能执行 Go 代码
P 有本地 run queue
G 会在 P 的队列、全局队列、netpoll、timer、GC worker 之间流转
go f() 大致发生：

newproc 创建 G
-> 放入当前 P 的本地队列
-> 必要时 wakep 唤醒/创建 M
-> M 绑定 P
-> schedule 找到可运行 G
-> 执行 f
调度器找活干的顺序大致是：

当前 P 的 runnext
-> 当前 P 的本地队列
-> 全局队列
-> netpoll 就绪事件
-> timer
-> 从其他 P 偷一半任务
-> 停车休眠
这就是为什么 goroutine 很便宜：它不是 OS 线程。大量 goroutine 可以复用少量 OS 线程。

## 5. 阻塞与抢占

如果 goroutine 做网络 I/O，Go 通常把 fd 设置成非阻塞，然后 goroutine park 到 netpoller，线程不会傻等。

如果 goroutine 进入阻塞 syscall：

G 进入 _Gsyscall
M 可能阻塞在内核
P 被释放给其他 M
其他 goroutine 继续跑
如果是 CPU 死循环，runtime 依赖安全点和异步抢占把它停下来，避免一个 goroutine 长时间霸占 P。底层有 sysmon 系统监控 goroutine，负责抢占、timer、netpoll、GC 辅助等维护工作。

## 6. 栈管理

每个 goroutine 有自己的栈，但不是 OS 线程那种几 MB 固定栈。Go 栈很小起步，按需增长。

函数入口附近有栈空间检查：

栈够 -> 继续执行
栈不够 -> morestack
-> 分配更大栈
-> 拷贝旧栈
-> 修正指针
-> 继续执行
runtime 里还有特殊的 g0 栈。调度、GC、栈扩容等 runtime 内部逻辑通常切到系统栈上执行，避免在普通 goroutine 栈上递归触发更多栈扩容。

## 7. 内存分配

Go 先由编译器做逃逸分析：

x := T{}
如果对象不逃逸，可能放栈上；逃逸了才进堆。

堆分配核心函数是 mallocgc。分配器结构大致是：

mcache   每个 P 的本地缓存，无锁快路径
mcentral 每种 size class 的 span 池
mheap    全局页堆
mspan    一段连续页，切成同尺寸对象
OS       VirtualAlloc / mmap 等系统内存
小对象通常走：

size class
-> 当前 P 的 mcache
-> 当前 mspan 找空位
-> 没有空位就找 mcentral
-> 再没有找 mheap
-> 再没有向 OS 要内存
大对象绕过 mcache/mcentral，直接从 mheap 分配。

Go 的小对象上限通常是 32KB 左右。你本机 Windows/amd64 的源码里，heap arena 尺寸比 Linux/macOS 小，Windows 64 位是 4MB arena，这是为了避免 Windows 提交内存成本过高。

## 8. GC 原理

Go 当前 GC 是：

并发
三色标记
精确 GC
非分代
非压缩
mark-sweep
核心阶段：

1. sweep termination：短 STW，结束上一轮清扫
2. mark setup：短 STW，打开写屏障
3. concurrent mark：并发标记，扫描栈、全局变量、堆对象
4. mark termination：短 STW，完成标记
5. concurrent sweep：并发清扫未使用对象
三色模型：

白色：还没确认存活，最后可能回收
灰色：确认存活，但它引用的对象还没扫完
黑色：确认存活，引用也处理完
写屏障的作用是：GC 并发标记时，用户 goroutine 还在改指针。如果没有屏障，GC 可能漏标对象。编译器会在指针写入处插入 runtime write barrier。

GOGC=100 的直觉是：上一轮 GC 后如果 live heap 是 100MB，那么大约再分配 100MB 后触发下一轮。官方 GC guide 里更精确的公式还会把 GC roots 算进去：

Target heap = Live heap + (Live heap + GC roots) * GOGC / 100
GOMEMLIMIT 是软内存上限，runtime 会用它调整 GC 频率和归还 OS 内存的行为。

## 9. channel / mutex 底层

channel 底层是 hchan，核心有：

环形缓冲区
send 等待队列
recv 等待队列
互斥锁
元素类型信息
发送时：

有接收者等待 -> 直接拷贝给接收者，唤醒它
有 buffer 空位 -> 放 buffer
都不行 -> 当前 G park 到 sendq
接收类似。park/unpark 不是 OS 线程阻塞，而是 G 状态切换，交给调度器。

sync.Mutex 快路径是 atomic CAS；竞争严重时走 runtime semaphore，把 goroutine park，避免一直烧 CPU。

## 10. race detector 和可能遇到的报错

go run -race 会插桩读写内存，依赖 runtime/race。Windows 下它需要 cgo，所以必须有 C 编译器。你刚才遇到的：

cgo: C compiler "gcc" not found
就是 runtime/cgo 需要 gcc，不是业务代码问题。装好 MSYS2 GCC 后，race detector 才能真正运行并报告数据竞争。

## 11. 最底层关键结构

runtime 源码里最重要的结构大概是：

g       goroutine，含栈、状态、调度现场
m       OS thread，含 g0、当前运行的 G、绑定的 P
p       processor，含本地 runq、mcache、timer、GC work buffer
schedt  全局调度器状态
mspan   一段堆页
mcache  P 本地小对象缓存
mcentral size class 级别 span 池
mheap   全局堆
调度切换不是普通函数调用，而是保存寄存器现场，切到 g0，再恢复另一个 G 的现场。runtime 里大量 //go:nosplit、//go:systemstack、汇编、原子操作，就是为了在“不能分配、不能增长栈、不能被抢占”的底层路径里保持正确性。

## 12. 实用心法

写 Go 时你大多数时候不需要直接碰 runtime。你真正需要记住的是：

goroutine 便宜，但不是免费
共享变量必须用 channel / mutex / atomic 同步
不要靠 sleep 等并发结果
减少无意义堆分配比手动 runtime.GC 更重要
先 pprof/trace，再调 GOGC/GOMEMLIMIT
finalizer 不是可靠资源释放机制
阻塞网络 I/O 通常没问题，阻塞 cgo/syscall 要小心
你可以重点读这几个本机源码入口：

C:/Users/Genius/sdk/go1.26.2/go/src/runtime/proc.go：GMP 调度器
C:/Users/Genius/sdk/go1.26.2/go/src/runtime/malloc.go：内存分配器
C:/Users/Genius/sdk/go1.26.2/go/src/runtime/mgc.go：GC 主流程


# 第一篇：Goroutine 深度解剖

## 1. 并发、并行与 Goroutine

### 1.1 基础用法演示

```go
go func() {
    fmt.Println("running in another goroutine")
}()
```

这行代码的表面含义是：启动一个新的执行单元。

但它不是直接创建一个系统线程，而是创建一个 **goroutine**。goroutine 由 Go runtime 调度，最终运行在少量 OS threads 上。

### 1.2 并发与并行

**并发 concurrency** 关注的是结构：

```text
多个任务在逻辑上同时推进
```

**并行 parallelism** 关注的是物理执行：

```text
多个任务真的在多个 CPU 核上同时运行
```

例如单核 CPU 上也可以并发：

```text
时间片 1：执行 goroutine A
时间片 2：执行 goroutine B
时间片 3：执行 goroutine A
```

但这不是并行。

多核 CPU 上可以并行：

```text
CPU0 -> goroutine A
CPU1 -> goroutine B
CPU2 -> goroutine C
```

### 1.3 为什么 Go 选择协程而不是直接使用系统线程

系统线程的成本较高：

- 创建销毁成本高。
- 默认栈通常较大。
- 调度由 OS 完成，runtime 难以做语言级优化。
- 大量阻塞线程会带来上下文切换压力。

Goroutine 的目标是：

```text
用更小的用户态执行单元承载海量并发任务
```

一个 goroutine 初始栈很小，通常只需要几 KB，并且可以动态增长。大量 goroutine 由 Go runtime 映射到少量 OS threads 上运行。

---

## 2. `go` 关键字底层发生了什么

### 2.1 基础用法演示

```go
func main() {
    go worker()
}

func worker() {
    fmt.Println("work")
}
```

`go worker()` 的本质不是“马上执行 worker”，而是：

```text
创建一个 goroutine 对象
把它放入可运行队列
等待调度器选择它运行
```

### 2.2 底层源码与数据结构解析

Go runtime 中创建 goroutine 的关键路径大致是：

```text
go func()
    -> runtime.newproc
    -> runtime.newproc1
    -> 创建 g
    -> 放入当前 P 的本地 runq
    -> 必要时唤醒或创建 M 执行
```
runtime.newproc & runtime.newproc1是什么
<details>

`runtime.newproc` 和 `runtime.newproc1` 是 **Go runtime 内部用来创建 goroutine 的函数**，不是你代码里直接调用的函数。

源码在这里：[/usr/local/go/src/runtime/proc.go](/usr/local/go/src/runtime/proc.go:5158)

大致关系是：

```go
go func() {
    // ...
}()
```

编译器会把这个 `go` 语句转换成类似：

```go
runtime.newproc(fn)
```

然后 runtime 负责真正创建 goroutine。

**`runtime.newproc` 是入口函数**

它主要做几件事：

```go
func newproc(fn *funcval) {
    gp := getg()              // 当前 goroutine
    pc := sys.GetCallerPC()   // 创建位置
    systemstack(func() {
        newg := newproc1(fn, gp, pc, false, waitReasonZero)

        pp := getg().m.p.ptr()
        runqput(pp, newg, true)

        if mainStarted {
            wakep()
        }
    })
}
```

可以理解为：

```text
newproc = 创建 goroutine 的外层入口
```

它会：

1. 获取当前 goroutine，也就是父 goroutine
2. 切换到系统栈执行
3. 调用 `newproc1` 真正创建新的 `g`
4. 把新 `g` 放入当前 `P` 的本地运行队列
5. 必要时调用 `wakep()` 唤醒/创建 `M` 来执行 goroutine

**`runtime.newproc1` 是真正干活的函数**

`newproc1` 做的是：

```text
创建并初始化一个新的 runtime.g 对象
```

也就是你笔记里的：

```text
创建 g
```

它会：

1. 从当前 `P` 的空闲 `g` 池里取一个旧的 `g`
2. 如果没有，就分配一个新的 `g`
3. 给这个 `g` 分配/准备栈
4. 设置它的启动函数，也就是 `go func()` 里的函数
5. 设置调度上下文，比如 `sp`、`pc`
6. 设置父 goroutine 信息、创建位置、goid
7. 把状态改成 `_Grunnable`

注意：`newproc1` **只负责创建和初始化 `g`**，它不会自己把 `g` 放进运行队列。源码注释里也写了：调用者负责把新 `g` 加入 scheduler。

所以你的这段可以理解成：

```text
go func()
    -> 编译器转成 runtime.newproc(fn)
    -> newproc 切到系统栈
    -> 调用 runtime.newproc1 真正创建一个新的 g
    -> newproc 把 newg 放入当前 P 的本地 runq
    -> wakep 唤醒 M 来执行
```

一句话总结：

```text
newproc 是“创建 goroutine 的入口”
newproc1 是“实际分配并初始化 g 的底层函数”
```

</details>


这里的 `g` 是 Go runtime 里的 goroutine 结构体，不是操作系统线程。操作系统线程对应的是 `M`，调度资源对应的是 `P`。

Runtime 中核心结构是 GMP：

```text
G = Goroutine，表示一个用户态执行任务
M = Machine，表示一个 OS thread
P = Processor，表示调度资源和本地运行队列
```

三者关系：

```text
        +---------+
        |    P    |  持有本地 runq、调度资源
        +----+----+
             |
             | 绑定
             v
        +---------+
        |    M    |  OS thread
        +----+----+
             |
             | 执行
             v
        +---------+
        |    G    |  goroutine
        +---------+
```

更准确地说：

```text
M 必须拿到 P，才能执行 Go 代码。
G 必须被调度到某个 M 上，才能真正运行。
P 决定 Go 代码并行度，数量由 GOMAXPROCS 控制。
```

---

## 3. GMP 模型深度剖析

### 3.1 基础用法演示

```go
runtime.GOMAXPROCS(4)
```

这表示最多有 4 个 P 同时执行 Go 代码。  
注意：这不是限制 goroutine 数量，也不是严格限制 OS thread 数量。

### 3.2 底层源码与结构体映射

官方 runtime 中有几个关键结构：

```go
// runtime/runtime2.go
type g struct { ... }
type m struct { ... }
type p struct { ... }
```

简化理解如下。

#### `g`：goroutine 的运行时对象

`g` 代表一个 goroutine，核心信息包括：

```text
stack       当前 goroutine 的栈范围
sched       保存 goroutine 被暂停时的寄存器状态
atomicstatus 当前状态，如 _Grunnable、_Grunning、_Gwaiting
goid        goroutine id，内部使用
waitreason  等待原因
m           当前运行它的 M
```

ASCII 示意：

```text
+----------------------+
| g                    |
+----------------------+
| stack: [lo, hi]      |
| sched: pc/sp/bp      |
| status: _Grunnable   |
| waitreason           |
| m -> current M       |
+----------------------+
```

#### `m`：OS thread 的抽象

`m` 代表一个系统线程，核心信息包括：

```text
g0       调度栈使用的特殊 goroutine
curg     当前正在运行的 goroutine
p        当前绑定的 P
nextp    准备接手的 P
spinning 是否正在自旋找任务
```

示意：

```text
+----------------------+
| m                    |
+----------------------+
| g0                   |
| curg -> g            |
| p -> P               |
| spinning             |
+----------------------+
```

#### `p`：调度资源

`p` 是调度器的核心资源，核心信息包括：

```text
runq        本地 goroutine 队列
runnext     优先运行的下一个 goroutine
status      _Pidle、_Prunning 等
m           当前绑定的 M
gFree       可复用的 goroutine 对象
```

示意：

```text
+-----------------------------+
| p                           |
+-----------------------------+
| runq: [g1, g2, g3, ...]     |
| runnext: g0                 |
| status: _Prunning           |
| m -> current M              |
+-----------------------------+
```

完整调度关系：

```mermaid
flowchart LR
    G1["G: goroutine"] --> RQ["P.local runq"]
    RQ --> P["P: processor"]
    P --> M["M: OS thread"]
    M --> CPU["CPU core"]
```

P如何创建以及与G，M的对应关系   
<details>
Go runtime 里通常会有多个 `P`。可以这样理解：

```text
P 的数量 = GOMAXPROCS
```

比如：

```text
GOMAXPROCS = 4
```

runtime 就会创建：

```text
P0
P1
P2
P3
```

所以可以看到：

```text
P0.runq: g1 g2 g3 g4
P1.runq:
P2.runq: g5 g6
P3.runq:
```

这是因为当前程序有 4 个 `P`。

**为什么要有多个 P？**

因为 Go 要支持 goroutine 并行执行。

在 GMP 模型里：

```text
G = goroutine
M = OS thread，操作系统线程
P = processor，调度上下文
```

一个 `M` 想执行 Go 代码，必须绑定一个 `P`。

如果只有一个 `P`：

```text
M0 + P0 -> 执行 goroutine
```

那同一时刻最多只有一个线程在执行 Go 代码。

如果有多个 `P`：

```text
M0 + P0 -> 执行 goroutine
M1 + P1 -> 执行 goroutine
M2 + P2 -> 执行 goroutine
M3 + P3 -> 执行 goroutine
```

那多个 goroutine 就可以真正并行跑在多个 CPU 核心上。

所以，多个 `P` 的意义是：

```text
控制 Go 代码的并行度
```

**P 是怎么创建的？**

Go 程序启动时，runtime 会初始化调度器。

源码大概在：

[/usr/local/go/src/runtime/proc.go](/usr/local/go/src/runtime/proc.go:832)

启动时会执行 `schedinit()`，里面会决定 `procs` 的数量：

```go
if n, ok := strconv.Atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
    procs = n
} else {
    procs = defaultGOMAXPROCS(numCPUStartup)
}
procresize(procs)
```

意思是：

1. 如果你设置了环境变量 `GOMAXPROCS`，就用你设置的值
2. 如果没设置，runtime 会根据机器 CPU 情况决定默认值
3. 然后调用 `procresize(procs)` 创建对应数量的 `P`

真正创建 `P` 的地方在：

[/usr/local/go/src/runtime/proc.go](/usr/local/go/src/runtime/proc.go:5866)

核心逻辑类似：

```go
if nprocs > int32(len(allp)) {
    allp = make([]*p, nprocs)
}

for i := old; i < nprocs; i++ {
    pp := allp[i]
    if pp == nil {
        pp = new(p)
    }
    pp.init(i)
    allp[i] = pp
}
```

也就是说，runtime 内部有一个全局数组/切片：

```go
allp []*p
```

如果 `GOMAXPROCS = 4`，那么大概就是：

```text
allp[0] = P0
allp[1] = P1
allp[2] = P2
allp[3] = P3
```

**每个 P 为什么有自己的 runq？**

因为如果所有 goroutine 都放进一个全局队列：

```text
global runq: g1 g2 g3 g4 g5 g6
```

多个线程同时取任务时，就要频繁抢同一把锁，性能会差。

所以 Go 给每个 `P` 一个本地运行队列：

```text
P0.runq
P1.runq
P2.runq
P3.runq
```

这样当前 `P` 创建出来的 goroutine，优先放到自己的本地队列里，减少锁竞争。

不过队列不一定平均，所以可能出现：

```text
P0.runq: g1 g2 g3 g4
P1.runq:
P2.runq: g5 g6
P3.runq:
```

这很正常。

如果 `P1` 空了，它不会傻等。它会尝试：

```text
1. 从全局 runq 拿
2. 从其他 P 的 runq 偷一部分 goroutine
3. 查网络轮询器 netpoll
4. 找定时器任务
```

这就是 work stealing，工作窃取。

一句话总结：

```text
P 的数量由 GOMAXPROCS 决定；
Go 程序启动时 runtime 通过 procresize 创建多个 P；
多个 P 是为了让多个 M 能并行执行 goroutine；
每个 P 有自己的本地 runq，是为了减少全局队列锁竞争。
```
</details>


### 3.3 常见踩坑与避坑指南

**坑 1：以为 goroutine 等于 thread。**

错误理解：

```text
启动 10000 个 goroutine = 启动 10000 个 OS threads
```

真实情况：

```text
10000 个 goroutine 通常复用少量 OS threads。
```

**坑 2：以为 `GOMAXPROCS` 控制 goroutine 数量。**

`GOMAXPROCS` 控制的是同时执行 Go 代码的 P 数量，不是 goroutine 上限。

**坑 3：无限创建 goroutine。**

goroutine 很轻，但不是免费。每个 goroutine 仍然需要：

```text
g 结构体
栈空间
调度开销
可能持有 channel、锁、网络连接等资源
```

---

## 4. 调度策略：Work Stealing 与 Hand Off

### 4.1 基础用法演示

```go
for i := 0; i < 1000; i++ {
    go func() {
        doWork()
    }()
}
```

这些 goroutine 不会平均地直接分配到所有线程。它们通常先进入当前 P 的本地队列。

### 4.2 Work Stealing：工作窃取

每个 P 有自己的本地 runq：

```text
P0.runq: g1 g2 g3 g4
P1.runq:
P2.runq: g5 g6
P3.runq:
```

如果 P1 没活干，它会尝试从其他 P 窃取一部分任务：

```text
P1 steals from P0
```

之后变成：

```text
P0.runq: g1 g2
P1.runq: g3 g4
P2.runq: g5 g6
P3.runq:
```

这就是 Work Stealing，目的：

```text
减少全局锁竞争
提高局部性
让空闲 P 尽快获得任务
```

调度器寻找任务的大致顺序：

```text
1. 当前 P 的 runnext
2. 当前 P 的本地 runq
3. 全局 runq
4. 网络轮询器 netpoll
5. 从其他 P 偷任务
```

### 4.3 Hand Off：系统调用时的 P 交接

当一个 goroutine 进入阻塞系统调用时：

```go
syscall.Read(...)
```

当前 M 可能被 OS syscall 卡住。

如果 M 一直持有 P，那么这个 P 上的其他 goroutine 就没法运行。  
所以 runtime 会把 P 从阻塞的 M 上剥离，交给别的 M。

示意：

```text
before syscall:

P0 -> M0 -> G1

G1 enters blocking syscall:

M0 blocked in kernel
P0 detached

P0 -> M1 -> G2
```

这就是 hand off 思路：

```text
M 被 syscall 卡住没关系
P 要被释放出来继续服务其他 goroutine
```

### 4.4 常见踩坑与避坑指南

**坑 1：CPU 密集 goroutine 不主动让出。**

Go 1.14 之后引入了更强的异步抢占，缓解长时间运行 goroutine 霸占 P 的问题。但写代码时仍应避免长时间无函数调用、无阻塞点的死循环。

**坑 2：阻塞调用绕过 runtime。**

如果通过 cgo 或某些外部调用长时间阻塞，runtime 的调度可见性会下降，可能造成线程膨胀或调度延迟。

---

## 5. Goroutine 栈内存：动态扩缩容

### 5.1 基础用法演示

你可以轻松创建大量 goroutine：

```go
for i := 0; i < 100000; i++ {
    go func() {
        time.Sleep(time.Minute)
    }()
}
```

这之所以可行，是因为 goroutine 栈不是像系统线程一样一开始就分配很大。

### 5.2 底层原理解析

goroutine 初始栈很小，随着调用深度增长自动扩容。

简化过程：

```text
函数调用前检查栈空间
空间不足 -> morestack
分配更大的栈
复制旧栈内容
修正指针
继续执行
```

示意：

```text
old stack:
+-----------+
| frames    |
+-----------+

grow:

new stack:
+-------------------+
| copied frames     |
| more free space   |
+-------------------+
```

早期 Go 用过 segmented stack，后来改成 contiguous stack，也就是连续栈扩容。这样避免了频繁跨段带来的性能问题。

### 5.3 常见踩坑与避坑指南

**坑 1：递归过深。**

goroutine 栈能增长，但不是无限增长。深递归仍然可能导致栈溢出或内存压力。

**坑 2：大量 goroutine 持有大对象引用。**

即使 goroutine 栈很小，只要它持有大对象引用，对象就不能被 GC 回收。

---

# 第二篇：Channel 深度解剖

## 6. CSP 模型与 Channel 的设计哲学

### 6.1 基础用法演示

```go
ch := make(chan int)

go func() {
    ch <- 42
}()

v := <-ch
fmt.Println(v)
```

channel 是 goroutine 之间传递数据的同步原语。

### 6.2 CSP 并发模型

CSP 的核心思想是：

```text
独立执行实体之间通过消息通信，而不是共享变量通信。
```

Go 的经典表达是：

```text
Do not communicate by sharing memory;
instead, share memory by communicating.
```

中文可以理解为：

```text
不要让多个 goroutine 共同抢着改同一份内存；
而是通过 channel 传递数据所有权或状态变化。
```

这不是说底层完全没有共享内存。channel 底层当然也有锁和队列。  
它的意思是：业务层尽量把共享状态封装在消息流后面。

### 6.3 常见踩坑与避坑指南

**坑 1：把 channel 当成万能替代锁。**

如果多个 goroutine 必须频繁读写同一个 map，`sync.Mutex` 往往更直接。

**坑 2：channel 传指针后继续并发修改。**

```go
ch <- &obj
obj.X = 10
```

如果接收方也在读写 `obj`，那仍然是共享内存竞争。

---

## 7. 无缓冲与有缓冲 Channel

### 7.1 基础用法演示

无缓冲 channel：

```go
ch := make(chan int)

go func() {
    ch <- 1 // 阻塞，直到有人接收
}()

v := <-ch
```

有缓冲 channel：

```go
ch := make(chan int, 2)

ch <- 1 // 不阻塞
ch <- 2 // 不阻塞
ch <- 3 // 阻塞，缓冲区满了
```

### 7.2 底层区别

无缓冲 channel：

```text
发送方和接收方必须直接交接
```

示意：

```text
sender G  --value--> receiver G
```

有缓冲 channel：

```text
发送方可以先把数据放入环形队列
接收方之后再取
```

示意：

```text
+-----------------------+
| buf[0] | buf[1] | ... |
+-----------------------+
   ^sendx      ^recvx
```

### 7.3 常见踩坑与避坑指南

**坑 1：以为有缓冲 channel 不会阻塞。**

缓冲区满了，发送仍然阻塞。  
缓冲区空了，接收仍然阻塞。

**坑 2：用 buffer 掩盖同步设计问题。**

把 `make(chan T)` 改成 `make(chan T, 1000)` 可能只是推迟问题爆发，不是修复问题。

---

## 8. `runtime.hchan` 结构体

### 8.1 基础用法演示

```go
ch := make(chan int, 3)
```

底层会创建一个 `runtime.hchan` 对象。

### 8.2 源码结构解析

官方 `runtime/chan.go` 中有：

```go
type hchan struct { ... }
```

关键字段可以简化为：

```text
qcount    当前队列中元素数量
dataqsiz  环形队列容量
buf       指向环形队列内存
elemsize  元素大小
closed    channel 是否已关闭
elemtype  元素类型
sendx     下一次发送写入位置
recvx     下一次接收读取位置
recvq     接收等待队列
sendq     发送等待队列
lock      保护 hchan 的 mutex
```

内存布局示意：

```text
+--------------------------------------------------+
| hchan                                            |
+--------------------------------------------------+
| qcount                                           |
| dataqsiz                                         |
| buf --------------------+                        |
| sendx                   |                        |
| recvx                   v                        |
| recvq -> sudog list   +----------------------+   |
| sendq -> sudog list   | ring buffer          |   |
| lock                  | [0][1][2][3]         |   |
+-----------------------+----------------------+---+
```

等待队列：

```text
recvq:
  sudog(G7) -> sudog(G8)

sendq:
  sudog(G3) -> sudog(G4)
```

`sudog` 是 runtime 用来表示“某个 goroutine 正在等待某个同步对象”的结构。  
channel 阻塞时，goroutine 自己不会直接挂在 channel 上，而是通过 `sudog` 挂入 `sendq` 或 `recvq`。

### 8.3 常见踩坑与避坑指南

**坑 1：拷贝 channel 变量不等于拷贝底层队列。**

```go
ch2 := ch
```

`ch` 和 `ch2` 指向同一个 `hchan`。

**坑 2：channel 的并发安全只限于 channel 操作本身。**

发送和接收是安全的，但发送的对象内部如果被多个 goroutine 改，仍然需要同步。

---

## 9. Channel Send 三大情景

### 9.1 基础用法演示

```go
ch <- v
```

### 9.2 底层流程解析

发送时 runtime 大致进入 `runtime.chansend`。

#### 情景一：有等待接收者，直接发送

```text
recvq 不为空
```

说明已经有 goroutine 阻塞在 `<-ch` 上。

流程：

```text
发送方拿到 hchan.lock
从 recvq 取出一个 sudog
把数据直接复制到接收方目标地址
唤醒接收方 goroutine
释放锁
```

示意：

```text
sender value
     |
     v
recv sudog.elem -> receiver stack slot
```

这种情况下，即使 channel 有 buffer，也优先直接交给等待接收者。

#### 情景二：没有接收者，但缓冲区未满

```text
recvq 为空
qcount < dataqsiz
```

流程：

```text
把元素复制到 buf[sendx]
sendx 前进
qcount++
释放锁
```

示意：

```text
buf:
[ A ][ B ][ _ ][ _ ]
          ^
        sendx
```

发送后：

```text
buf:
[ A ][ B ][ V ][ _ ]
              ^
            sendx
```

#### 情景三：没有接收者，缓冲区也满了

```text
recvq 为空
qcount == dataqsiz
```

流程：

```text
创建 sudog
记录当前 G、发送数据地址、channel 等信息
把 sudog 挂入 sendq
当前 G 状态变为 waiting
调用 gopark 休眠
```

示意：

```text
sendq:
sudog{g: G12, elem: &v} -> sudog{g: G13, elem: &v2}
```

### 9.3 常见踩坑与避坑指南

**坑 1：向 nil channel 发送永久阻塞。**

```go
var ch chan int
ch <- 1 // deadlock
```

**坑 2：向 closed channel 发送 panic。**

```go
close(ch)
ch <- 1 // panic: send on closed channel
```

---

## 10. Channel Receive 情景详解

### 10.1 基础用法演示

```go
v := <-ch
v, ok := <-ch
```

`ok` 用来判断 channel 是否已经关闭且数据已取完。

### 10.2 底层流程解析

接收时 runtime 大致进入 `runtime.chanrecv`。

#### 情景一：有等待发送者

```text
sendq 不为空
```

如果是无缓冲 channel：

```text
直接从发送方 sudog.elem 拷贝到接收方变量
唤醒发送方
```

如果是有缓冲 channel 且 buffer 非空，会涉及：

```text
接收方从 buf[recvx] 取数据
等待发送方的数据进入 buf[sendx]
唤醒发送方
```

这样可以保持环形队列语义。

#### 情景二：缓冲区有数据

```text
qcount > 0
```

流程：

```text
从 buf[recvx] 拷贝数据
清空该 slot
recvx 前进
qcount--
释放锁
```

#### 情景三：没有数据，channel 未关闭

```text
qcount == 0
sendq 为空
closed == false
```

接收方阻塞：

```text
创建 sudog
挂入 recvq
gopark
等待发送者或 close 唤醒
```

#### 情景四：channel 已关闭且无数据

```go
v, ok := <-ch
```

返回：

```text
v = 元素类型零值
ok = false
```

### 10.3 常见踩坑与避坑指南

**坑 1：从 closed channel 接收不会 panic。**

```go
close(ch)
v, ok := <-ch // ok == false
```

**坑 2：range channel 必须有人 close。**

```go
for v := range ch {
    fmt.Println(v)
}
```

如果没人关闭 `ch`，循环可能永远阻塞。

---

## 11. Close 的底层动作与广播机制

### 11.1 基础用法演示

```go
close(ch)
```

关闭 channel 表示：

```text
不会再有新值发送进来
接收方可以继续取出缓冲区已有数据
缓冲区取完后，接收零值且 ok=false
```

### 11.2 底层动作解析

`closechan` 大致做这些事：

```text
拿 hchan.lock
检查是否 nil 或已关闭
设置 closed = 1
唤醒所有 recvq 中的接收者
唤醒所有 sendq 中的发送者
释放锁
```

注意区别：

```text
被 close 唤醒的接收者：返回零值，ok=false
被 close 唤醒的发送者：panic
```

示意：

```text
close(ch)

recvq: G1 G2 G3  -> 全部唤醒，接收零值
sendq: G4 G5     -> 全部唤醒，panic: send on closed channel
```

### 11.3 常见踩坑与避坑指南

**坑 1：重复 close panic。**

```go
close(ch)
close(ch) // panic: close of closed channel
```

**坑 2：接收方不要随便 close。**

通常遵循：

```text
谁发送，谁关闭
多个发送者时，需要额外协调后由唯一 goroutine 关闭
```

---

# 第三篇：实战与避坑指南

## 12. Select 机制：多路复用

### 12.1 基础用法演示

```go
select {
case v := <-ch1:
    fmt.Println(v)
case ch2 <- 10:
    fmt.Println("sent")
case <-time.After(time.Second):
    fmt.Println("timeout")
}
```

`select` 用于同时等待多个 channel 操作。

### 12.2 底层原理解析

Runtime 中核心入口是 `runtime.selectgo`。

大致流程：

```text
1. 收集所有 case
2. 随机打乱 poll 顺序，避免固定优先级饥饿
3. 按 channel 地址排序加锁，避免死锁
4. 第一轮扫描：有没有立即可执行的 case
5. 如果有，执行并返回
6. 如果没有 default，构造 sudog 挂到多个 channel 的等待队列
7. gopark 阻塞
8. 被某个 channel 唤醒后，从其他 channel 队列中撤销等待
9. 执行命中的 case
```

示意：

```text
G waiting on select
   |
   +-- sudog -> ch1.recvq
   +-- sudog -> ch2.sendq
   +-- sudog -> ch3.recvq
```

一个 goroutine 可以通过多个 `sudog` 同时挂到多个 channel 上，但最终只能被一个 case 唤醒。

### 12.3 常见踩坑与避坑指南

**坑 1：`select {}` 永久阻塞。**

```go
select {}
```

这会让当前 goroutine 永远睡眠。

**坑 2：`default` 造成忙轮询。**

```go
for {
    select {
    case v := <-ch:
        use(v)
    default:
    }
}
```

这个循环会疯狂占 CPU。通常应加 sleep、ticker，或重新设计阻塞逻辑。

---

## 13. Goroutine 泄漏图谱

### 13.1 基础示例

```go
func leak() {
    ch := make(chan int)

    go func() {
        ch <- 1
    }()
}
```

如果没人接收，goroutine 永久阻塞在发送上。

### 13.2 常见泄漏场景

```text
1. 向无人接收的 channel 发送
2. 从无人发送且未关闭的 channel 接收
3. range 一个永远不会 close 的 channel
4. worker 没有退出信号
5. context 超时后，下游 goroutine 没有监听 Done
6. select 中 nil channel 被误用导致永久屏蔽
7. 定时器、ticker 没有 Stop
```

排查方式：

```go
pprof.Lookup("goroutine").WriteTo(os.Stdout, 2)
```

或者：

```bash
go tool pprof
```

重点看 goroutine 卡在哪里：

```text
chan send
chan receive
select
sync.Cond.Wait
sync.Mutex.Lock
netpoll
```

### 13.3 避坑指南

为长期 goroutine 提供退出路径：

```go
func worker(ctx context.Context, jobs <-chan Job) {
    for {
        select {
        case <-ctx.Done():
            return
        case job, ok := <-jobs:
            if !ok {
                return
            }
            handle(job)
        }
    }
}
```

---

## 14. Channel Panic 与 Deadlock 总结

### 14.1 会 panic 的情况

```go
close(nilChan)          // panic
close(closedChan)       // panic
send on closed channel  // panic
```

示例：

```go
ch := make(chan int)
close(ch)
ch <- 1
```

### 14.2 不会 panic 但可能阻塞的情况

```go
var ch chan int

ch <- 1  // 永久阻塞
<-ch     // 永久阻塞
```

nil channel 常用于动态禁用 select case：

```go
var ch <-chan int

select {
case v := <-ch:
    fmt.Println(v)
case <-time.After(time.Second):
    fmt.Println("timeout")
}
```

### 14.3 Deadlock 常见情况

```go
func main() {
    ch := make(chan int)
    ch <- 1
}
```

主 goroutine 阻塞，且没有其他 goroutine 能唤醒它，runtime 会报：

```text
fatal error: all goroutines are asleep - deadlock!
```

但如果程序里还有网络轮询、定时器或其他可运行 goroutine，不一定立刻报 deadlock。

---

## 15. 高级并发范式

### 15.1 Worker Pool

```go
type Job struct {
    ID int
}

func worker(ctx context.Context, jobs <-chan Job, results chan<- int) {
    for {
        select {
        case <-ctx.Done():
            return
        case job, ok := <-jobs:
            if !ok {
                return
            }
            results <- job.ID * 2
        }
    }
}

func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    jobs := make(chan Job)
    results := make(chan int)

    for i := 0; i < 4; i++ {
        go worker(ctx, jobs, results)
    }

    go func() {
        defer close(jobs)
        for i := 0; i < 10; i++ {
            jobs <- Job{ID: i}
        }
    }()
}
```

Worker Pool 的核心：

```text
固定 worker 数量
用 channel 分发任务
用 context 控制退出
由发送方关闭 jobs
```

### 15.2 Pipeline

```go
func gen(nums ...int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for _, n := range nums {
            out <- n
        }
    }()
    return out
}

func square(in <-chan int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for n := range in {
            out <- n * n
        }
    }()
    return out
}

func main() {
    for v := range square(gen(1, 2, 3)) {
        fmt.Println(v)
    }
}
```

Pipeline 的核心：

```text
每一阶段拥有自己的输入输出 channel
上游 close 输出
下游 range 输入
数据沿 channel 单向流动
```

---

# 结语：Go 并发的真正边界

Go 并发的核心不是“开很多 goroutine”，而是正确建模：

```text
Goroutine 负责并发执行
Channel 负责通信与同步
Mutex 负责保护共享状态
Context 负责生命周期取消
Runtime 调度器负责把 G 映射到 M/P 上执行
```

最重要的实践判断是：

```text
数据所有权可以流动 -> 优先考虑 channel
多个 goroutine 必须共享同一份状态 -> 使用 mutex
需要等待某个状态变化 -> mutex + cond
需要限制并发数 -> semaphore 或 worker pool
需要取消整条调用链 -> context
```

参考源码与资料：

- [Go runtime: runtime2.go](https://go.dev/src/runtime/runtime2.go)
- [Go runtime: proc.go](https://go.dev/src/runtime/proc.go)
- [Go runtime: chan.go](https://go.dev/src/runtime/chan.go)
- [Go runtime: select.go](https://go.dev/src/runtime/select.go)
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share)