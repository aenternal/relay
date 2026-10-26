# Descriptor import fixture

`editor.protoset` is a binary `google.protobuf.FileDescriptorSet` compiled from
`editor.proto` by Relay's `importer.Compile` and exported by
`registry.DescriptorSet`. It uses the pinned `protocompile` and Protobuf Go
versions in `go.mod`, deterministic `proto.MarshalOptions`, and includes source
comments. No external `protoc` or generated client code was used.

SHA-256: `914bbe0afa83c167eb0e185bd304133c8067e81166302af7e216b7b8696296b8`.

The fixture exposes `/relay.descriptor.v1.DescriptorService/Echo`, with
`relay.descriptor.v1.Ping` as input and output. Its `sequence` field verifies exact
64-bit integer conversion. Import, inspection and body validation run offline;
the fixture starts no server and performs no RPC.
