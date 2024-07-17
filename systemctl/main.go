package systemctl

import (
	"bytes"
	"docker-systemd/common"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
)

/* send buffer is:
16-bit int: size of array
for each slice:
	16-bit int: size of string
	string
*/

/* receive buffer is:
string: to print out followed by 0x00 byte
-- send 0x00 in response
magic 5-byte signature and 16-bit int: return value
*/

func Main(args []string) {
	conn, err := net.Dial("unix", common.SocketPath())
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	sendBuf := []byte{}
	sendSize := 2
	sendBuf = binary.LittleEndian.AppendUint16(sendBuf, uint16(len(args)-1))
	for i := range args {
		if i == 0 {
			continue
		}
		sendSize += 2
		sendSize += len(args[i])
		sendBuf = binary.LittleEndian.AppendUint16(sendBuf, uint16(len(args[i])))
		sendBuf = append(sendBuf, []byte(args[i])...)
	}
	if _, err := conn.Write([]byte(sendBuf[0:sendSize])); err != nil {
		log.Fatal(err)
	}
	recvBuf := make([]byte, 65536)

	isEnd := false
	for {
		recvSize, err := conn.Read(recvBuf)
		if err != nil {
			log.Fatal(err)
		}
		if !isEnd && bytes.HasSuffix(recvBuf[0:recvSize], []byte{0x00}) {
			if recvSize > 1 {
				fmt.Print(string(recvBuf[0 : recvSize-1]))
			}
			_, err = conn.Write([]byte{0x00})
			if err != nil {
				log.Fatalf("ERROR sending readiness for response code signal: %s", err)
			}
			isEnd = true
			continue
		}
		if isEnd {
			if recvSize >= 5 && recvSize <= 7 && bytes.Equal(recvBuf[0:5], common.SystemctlExitCodeMagic) {
				os.Exit(int(binary.LittleEndian.Uint16(recvBuf[recvSize-2 : recvSize])))
			}
			log.Fatalf("Received extra bytes at end of message: %v", recvBuf[0:recvSize])
		}
		fmt.Print(string(recvBuf[0:recvSize]))
	}
}
