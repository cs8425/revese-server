# revese-server

simple tool for expose a local server behind a NAT or firewall to the internet.


## hub

a server on the internet that any one can connect.

* upstreamaddr : connect for service to expose
* localaddr : bind for normal client


## client

behind NAT, can connect to service which needs to expose.

* targetaddr : connect to service to expose
* hubaddr : connect to `hub`

## example config

* PC A : behind NAT, has a service on :8080
  * run `client -c client.json`
* PC B : on the internet, expose service on :80
  * run `hub -c hub.json`

````
       port 80   |        |  port 10999  |          port 3001
user  -------->  |  hub   |  <=========  |  client ----------->  services
                 |  PC A  |              |  PC B
````

