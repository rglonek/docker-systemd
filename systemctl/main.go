package systemctl

import (
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
16-bit int: return value
string: to print out
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
	if recvSize, err := conn.Read(recvBuf); err != nil {
		log.Fatal(err)
	} else if recvSize < 2 {
		log.Fatal("receive error: data too small")
	} else if recvSize == 2 {
		os.Exit(int(binary.LittleEndian.Uint16(recvBuf[0:2])))
	} else if recvSize > 2 {
		fmt.Println(string(recvBuf[2:recvSize]))
		os.Exit(int(binary.LittleEndian.Uint16(recvBuf[0:2])))
	}
}
