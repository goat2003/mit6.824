# Go 并发编程深度指南：Goroutine 与 Channel 原理全解析

> 说明：本文基于 Go 1.14+ 的运行时模型，并参考当前官方源码中的 `runtime.g`、`runtime.m`、`runtime.p`、`runtime.hchan`、`runtime.sudog`、`selectgo` 等结构。Runtime 内部字段会随版本变化，但核心模型稳定。

---

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