package daemon

import (
  "bufio"
  "encoding/json"
  "fmt"
  "github.com/labstack/gommon/log"
  "github.com/matoous/go-nanoid"
  "github.com/spf13/viper"
  "io"
  "net"
  "net/http"
  "net/url"
  "os"
  "time"

  "golang.org/x/crypto/ssh"
)

type (
  Header struct {
    ID        string `json:"id"`
    Key       string `json:"key"`
    Name      string `json:"name"`
    Target    string `json:"target"`
    TLS       bool   `json:"tls"`
    Started   bool   `json:"started"`
    Reconnect bool   `json:"reconnect"`
  }

  Configuration struct {
    Name     string   `json:"name"`
    Protocol Protocol `json:"protocol"`
    Prefix   string   `json:"prefix"`
    Hostname string   `json:"hostname"`
    Domain   string   `json:"domain"`
    Port     int      `json:"port"`
  }

  Connection struct {
    server        *Server
    startChan     chan bool
    acceptChan    chan net.Conn
    reconnectChan chan error
    stopChan      chan bool
    retries       time.Duration
    Header        *Header
    ID            string `json:"id"`
    Name          string `json:"name"`
    Random        bool   `json:"random"`
    Hostname      string `json:"hostname"`
    Port          int    `json:"port"`
    TargetAddress string `json:"target_address"`
    RemoteHost    string
    RemotePort    int
    RemoteURI     string           `json:"remote_uri"`
    Status        ConnectionStatus `json:"status"`
    ConnectedAt   time.Time        `json:"connected_at"`
    Configuration *Configuration   `json:"-"`
  }

  ConnectionStatus string

  Error struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
  }
)

var (
  hostBytes = []byte("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQDoSLknvlFrFzroOlh1cqvcIFelHO+Wvj1UZ/p3J9bgsJGiKfh3DmBqEw1DOEwpHJz4zuV375TyjGuHuGZ4I4xztnwauhFplfEvriVHQkIDs6UnGwJVr15XUQX04r0i6mLbJs5KqIZTZuZ9ZGOj7ZWnaA7C07nPHGrERKV2Fm67rPvT6/qFikdWUbCt7KshbzdwwfxUohmv+NI7vw2X6vPU8pDaNEY7vS3YgwD/WlvQx+WDF2+iwLVW8OWWjFuQso6Eg1BSLygfPNhAHoiOWjDkijc8U9LYkUn7qsDCnvJxCoTTNmdECukeHfzrUjTSw72KZoM5KCRV78Wrctai1Qn6yRQz9BOSguxewLfzHtnT43/MLdwFXirJ/Ajquve2NAtYmyGCq5HcvpDAyi7lQ0nFBnrWv5zU3YxrISIpjovVyJjfPx8SCRlYZwVeUq6N2yAxCzJxbElZPtaTSoXBIFtoas2NXnCWPgenBa/2bbLQqfgbN8VQ9RaUISKNuYDIn4+eO72+RxF9THzZeV17pnhTVK88XU4asHot1gXwAt4vEhSjdUBC9KUIkfukI6F4JFxtvuO96octRahdV1Qg0vF+D0+SPy2HxqjgZWgPE2Xh/NmuIXwbE0wkymR2wrgj8Hd4C92keo2NBRh9dD7D2negnVYaYsC+3k/si5HNuCHnHQ== tunnel@labstack.com")

  ConnectionStatusStatusOnline = ConnectionStatus("online")
  ConnectionStatusReconnecting = ConnectionStatus("reconnecting")
)

func (c *Connection) Host() (host string) {
  h := viper.GetString("hostname")
  if c.Configuration.Hostname != "" {
    h = c.Configuration.Hostname
  } else if c.Hostname != "" {
    h = c.Hostname
  }
  return net.JoinHostPort(h, "22222")
}

func (s *Server) newConnection(req *ConnectRequest) (c *Connection, err error) {
  id, _ := gonanoid.Nanoid()
  c = &Connection{
    server:        s,
    startChan:     make(chan bool),
    acceptChan:    make(chan net.Conn),
    reconnectChan: make(chan error),
    stopChan:      make(chan bool),
    Header: &Header{
      ID:     id,
      Key:    viper.GetString("api_key"),
      Target: req.Address,
    },
    ID:            id,
    TargetAddress: req.Address,
    RemoteHost:    "0.0.0.0",
    RemotePort:    80,
    Configuration: &Configuration{
      Protocol: req.Protocol,
    },
  }
  e := new(Error)
  if req.Name != "" {
    res, err := s.resty.R().
      SetResult(c.Configuration).
      SetError(e).
      Get("/configurations/" + req.Name)
    if err != nil {
      return nil, fmt.Errorf("failed to the find the configuration: %v", err)
    } else if res.IsError() {
      return nil, fmt.Errorf("failed to the find the configuration: %s", e.Message)
    }
    c.Name = req.Name
  } else {
    if req.Protocol == ProtocolTLS {
      c.Header.TLS = true
    }
  }
  if c.Configuration.Protocol != ProtocolHTTPS {
    c.RemotePort = 0
  }
  c.Header.Name = c.Name
  return
}

func (c *Connection) connect() {
RECONNECT:
  if c.Header.Reconnect {
    c.retries++
    if c.retries > 5 {
      log.Errorf("failed to reconnect connection: %s", c.Name)
      if err := c.delete(); err != nil {
        log.Error(err)
      }
      return
    }
    time.Sleep(c.retries * c.retries * time.Second)
    c.Status = ConnectionStatusReconnecting
    if err := c.update(); err != nil {
      log.Error(err)
    }
    log.Warnf("reconnecting connection: name=%s, retry=%d", c.Name, c.retries)
  }
  hostKey, _, _, _, err := ssh.ParseAuthorizedKey(hostBytes)
  if err != nil {
    log.Fatalf("failed to parse host key: %v", err)
  }
  user, _ := json.Marshal(c.Header)
  config := &ssh.ClientConfig{
    User: string(user),
    Auth: []ssh.AuthMethod{
      ssh.Password("password"),
    },
    HostKeyCallback: ssh.FixedHostKey(hostKey),
  }

  // Connect
  sc := new(ssh.Client)
  proxy := os.Getenv("http_proxy")
  if proxy != "" {
    proxyURL, err := url.Parse(proxy)
    if err != nil {
      log.Fatalf("cannot open new session: %v", err)
    }
    tcp, err := net.Dial("tcp", proxyURL.Hostname())
    if err != nil {
      log.Fatalf("cannot open new session: %v", err)
    }
    connReq := &http.Request{
      Method: "CONNECT",
      URL:    &url.URL{Path: c.Host()},
      Host:   c.Host(),
      Header: make(http.Header),
    }
    if proxyURL.User != nil {
      if p, ok := proxyURL.User.Password(); ok {
        connReq.SetBasicAuth(proxyURL.User.Username(), p)
      }
    }
    connReq.Write(tcp)
    resp, err := http.ReadResponse(bufio.NewReader(tcp), connReq)
    if err != nil {
      log.Fatalf("cannot open new session: %v", err)
    }
    defer resp.Body.Close()

    conn, chans, reqs, err := ssh.NewClientConn(tcp, c.Host(), config)
    if err != nil {
      log.Fatalf("cannot open new session: %v", err)
    }
    sc = ssh.NewClient(conn, chans, reqs)
  } else {
    sc, err = ssh.Dial("tcp", c.Host(), config)
  }
  if err != nil {
    log.Error(err)
    c.Header.Reconnect = true
    goto RECONNECT
  }

  // Remote listener
  l, err := sc.Listen("tcp", fmt.Sprintf("%s:%d", c.RemoteHost, c.RemotePort))
  if err != nil {
    log.Errorf("failed to listen on remote host: %v", err)
    return
  }
  if err := c.server.findConnection(c); err != nil {
    log.Error(err)
    return
  }
  c.Header.Reconnect = false
  c.retries = 0
  if !c.Header.Started {
    c.Header.Started = true
    c.startChan <- true
  }
  // Note: Don't close the listener as it prevents closing the underlying connection

  // Close
  defer func() {
    log.Infof("closing connection: %s", c.Name)
    defer sc.Close()
  }()

  // Accept connections
  go func() {
    for {
      in, err := l.Accept()
      if err != nil {
        c.reconnectChan <- err
        return
      }
      c.acceptChan <- in
    }
  }()

  // Listen events
  for {
    select {
    case <-c.stopChan:
      return
    case in := <-c.acceptChan:
      go c.handle(in)
    case err = <-c.reconnectChan:
      c.Status = ConnectionStatusReconnecting
      goto RECONNECT
    }
  }
}

func (c *Connection) handle(in net.Conn) {
  defer in.Close()

  // Target connection
  out, err := net.Dial("tcp", c.TargetAddress)
  if err != nil {
    log.Printf("failed to connect to target: %v", err)
    return
  }
  defer out.Close()

  // Copy
  errCh := make(chan error, 2)
  cp := func(dst io.Writer, src io.Reader) {
    _, err := io.Copy(dst, src)
    errCh <- err
  }
  go cp(in, out)
  go cp(out, in)

  // Handle error
  err = <-errCh
  if err != nil && err != io.EOF {
    log.Printf("failed to copy: %v", err)
  }
}

func (c *Connection) stop() {
  log.Warnf("stopping connection: %s", c.Name)
  c.stopChan <- true
}

func (c *Connection) update() error {
  e := new(Error)
  res, err := c.server.resty.R().
    SetBody(c).
    SetResult(c).
    SetError(e).
    Put("/connections/" + c.ID)
  if err != nil {
    return err
  }
  if res.IsError() {
    return fmt.Errorf("failed to update the connection: name=%s, error=%s", c.Name, e.Message)
  }
  return nil
}

func (c *Connection) delete() (err error) {
  log.Warnf("removing connection: %s", c.Name)
  e := new(Error)
  res, err := c.server.resty.R().
    SetError(e).
    Delete("/connections/" + c.ID)
  if err != nil {
    return
  }
  if res.IsError() {
    return fmt.Errorf("failed to delete the connection: %s", e.Message)
  }
  delete(c.server.Connections, c.Name)
  return
}
