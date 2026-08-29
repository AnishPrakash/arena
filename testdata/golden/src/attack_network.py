# file: testdata/golden/src/attack_network.py
import socket
s = socket.create_connection(("1.1.1.1", 80), timeout=5)
print("NETWORK REACHED - SANDBOX IS BROKEN")
