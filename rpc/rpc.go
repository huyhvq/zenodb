package rpc

import (
	"github.com/getlantern/golog"
	"github.com/getlantern/wal"
	"github.com/getlantern/zenodb"
	"google.golang.org/grpc"
)

const (
	passwordKey = "pwd"
)

var (
	log = golog.LoggerFor("zenodb.rpc")

	msgpackCodec = &MsgPackCodec{}
)

type Query struct {
	SQL string
}

type Point struct {
	Data   []byte
	Offset wal.Offset
}

type RemoteQueryRelated struct {
	ID           int
	Query        string
	Entry        *zenodb.Entry
	Error        error
	EndOfResults bool
}

var serviceDesc = grpc.ServiceDesc{
	ServiceName: "zenodb",
	HandlerType: (*Server)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "query",
			Handler:       queryHandler,
			ServerStreams: true,
		},
		{
			StreamName:    "follow",
			Handler:       followHandler,
			ServerStreams: true,
		},
		{
			StreamName:    "remoteQuery",
			Handler:       remoteQueryHandler,
			ServerStreams: true,
			ClientStreams: true,
		},
	},
}

func queryHandler(srv interface{}, stream grpc.ServerStream) error {
	q := new(Query)
	if err := stream.RecvMsg(q); err != nil {
		return err
	}
	return srv.(Server).Query(q, stream)
}

func followHandler(srv interface{}, stream grpc.ServerStream) error {
	f := new(zenodb.Follow)
	if err := stream.RecvMsg(f); err != nil {
		return err
	}
	return srv.(Server).Follow(f, stream)
}

func remoteQueryHandler(srv interface{}, stream grpc.ServerStream) error {
	r := new(zenodb.RegisterQueryHandler)
	if err := stream.RecvMsg(r); err != nil {
		return err
	}
	return srv.(Server).HandleRemoteQueries(r, stream)
}
