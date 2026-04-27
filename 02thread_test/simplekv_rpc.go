package main

import (
	"fmt"
	"log"
	"net"
	"net/rpc"
	"sync"
)


//定义请求和回复参数结构体
const (
	OK       = "OK"
	ErrNoKey = "ErrNoKey"
)

type Err string

type PutArgs struct {
	Key   string
	Value string
}

type PutReply struct {
	Err Err
}

type GetArgs struct {
	Key string
}

type GetReply struct {
	Err   Err
	Value string
}


//客户端

func connect() *rpc.Client {
	client, err := rpc.Dial("tcp", ":1234")
	if err != nil {
		log.Fatal("dialing:", err)
	}
	return client
}

func get(key string) string {
	client := connect()
	args := GetArgs{key}
	reply := GetReply{}
	// 发起 RPC 调用：调用远端 KV 结构体的 Get 方法
	err := client.Call("KV.Get", &args, &reply)
	if err != nil {
		log.Fatal("error:", err)
	}
	client.Close()
	return reply.Value
}

func put(key string, val string) {
	client := connect()
	args := PutArgs{key, val}
	reply := PutReply{}
	// 发起 RPC 调用：调用远端 KV 结构体的 Put 方法
	err := client.Call("KV.Put", &args, &reply)
	if err != nil {
		log.Fatal("error:", err)
	}
	client.Close()
}



// 3. 服务端 (Server)

type KV struct {
	mu   sync.Mutex
	data map[string]string
}

func (kv *KV) Get(args *GetArgs, reply *GetReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	val, ok := kv.data[args.Key]
	if ok {
		reply.Err = OK
		reply.Value = val
	} else {
		reply.Err = ErrNoKey
		reply.Value = ""
	}
	return nil
}

func (kv *KV) Put(args *PutArgs, reply *PutReply) error {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.data[args.Key] = args.Value
	reply.Err = OK
	return nil
}

func server() {
	kv := new(KV)
	kv.data = map[string]string{}
	
	// 注册 RPC 服务
	rpcs := rpc.NewServer()
	rpcs.Register(kv)
	
	// 监听本地 1234 端口
	l, e := net.Listen("tcp", ":1234")
	if e != nil {
		log.Fatal("listen error:", e)
	}
	
	// 开启一个 Goroutine 处理连接
	go func() {
		for {
			conn, err := l.Accept()
			if err == nil {
				// 为每一个进来的请求开启一个新的 Goroutine 提供 RPC 服务
				go rpcs.ServeConn(conn)
			} else {
				break
			}
		}
	}()
}



func main() {
	server()

	put("subject", "6.824")
	fmt.Printf("Put(subject, 6.824) done\n")
	fmt.Printf("get(subject) -> %s\n", get("subject"))
}