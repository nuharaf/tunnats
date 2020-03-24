# tunnats
**tunnats** is tcp tunnel that use nats server for transport. You can use it on various use case such as tunneling ssh on your remote machine as an alternative to ngrok or localhost.run.
**tunnats** also work with Synadia NGS service, with their free tier you can have free tcp tunneling.

#### Concept
**tunnats** consist of two component,an agent which run on the target machine and a client which run on the local machine. Tunnel is created by invoking http api on the client.
The API take remote_host and remote_port parameter which will became the destination address agent will connect. Parameter local_host and local_port determine where the client will create socket listener.

#### Installation
Currently , only the source code is provided. tunnats need to be installed on both target and local machine.
```
go get github.com/nuhamind2/tunnats
```
#### Running
Signup to Synadia NGS service [here](https://synadia.com/ngs/signup) and obtain the credential file required to connect to the service.You can find the creds file on ~/.nkeys folder.
Copy the creds file to both target machine and local machine then edit the creds_path in config.json.

Run the agent
```
go run main.go agent
```

Run the client
```
go run main.go client
```

#### Creating Tunnel
Invoke the http api on the tunnats client, this example use [httpie](httpie.org)
```
http localhost:8080/create_listener/123456789 remote_host=localhost remote_port=22 local_host=localhost local_port=10000
```
this api will create tunnel between agent with agentID = 123456789 on localhost:22 (respective to the agent machine) to the client on localhost:10000 (respective to the client machine)

Run ssh on this port and you should connect to the remote machine
```
ssh localhost -p 10000
```

#### Troubleshooting
TODO





