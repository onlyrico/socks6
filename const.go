package socks6

const (
	// TODO: IANA consideration
	SOCKS6CleartextPort = 1080
	SOCKS6EncryptedPort = 8389
)

const (
	AuthenticationMethodNone             byte = 0
	AuthenticationMethodGSSAPI           byte = 1
	AuthenticationMethodUsernamePassword byte = 2
)

const (
	CommandNoop byte = iota
	CommandConnect
	CommandBind
	CommandUdpAssociate
)

const (
	AuthenticationReplySuccess = 0
	AuthenticationReplyFail    = 1
)

const (
	OperationReplySuccess byte = iota
	OperationReplyServerFailure
	OperationReplyNotAllowedByRule
	OperationReplyNetworkUnreachable
	OperationReplyHostUnreachable
	OperationReplyConnectionRefused
	OperationReplyTTLExpired
	OperationReplyCommandNotSupported
	OperationReplyAddressNotSupported
	OperationReplyTimeout
)

const (
	_ byte = iota
	UDPMessageAssociationInit
	UDPMessageAssociationAck
	UDPMessageDatagram
	UDPMessageError
)
