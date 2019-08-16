/* Copyright (c) 2018, joy.zhou <chowyu08@gmail.com>
 */
package broker

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fhmq/rhmq/broker/lib/sessions"
	"github.com/fhmq/rhmq/broker/lib/topics"
	"github.com/fhmq/rhmq/plugins/bridge"

	"github.com/eclipse/paho.mqtt.golang/packets"
	"go.uber.org/zap"
)

const (
	// special pub topic for cluster info BrokerInfoTopic
	BrokerInfoTopic = "broker000100101info"
	// CLIENT is an end user.
	CLIENT = 0
	// ROUTER is client in the router.
	ROUTER = 1
)

const (
	_GroupTopicRegexp = `^\$share/([0-9a-zA-Z_-]+)/(.*)$`
)

const (
	Connected    = 1
	Disconnected = 2
)

var (
	groupCompile = regexp.MustCompile(_GroupTopicRegexp)
)

type client struct {
	typ        int
	mu         sync.Mutex
	broker     *Broker
	conn       net.Conn
	info       info
	status     int
	ctx        context.Context
	cancelFunc context.CancelFunc
	session    *sessions.Session
	subMap     map[string]*subscription
	topicsMgr  *topics.Manager
	rmsgs      []*packets.PublishPacket
}

type subscription struct {
	client    *client
	topic     string
	qos       byte
	share     bool
	groupName string
}

type info struct {
	clientID  string
	username  string
	password  []byte
	keepalive uint16
	willMsg   *packets.PublishPacket
	localIP   string
	remoteIP  string
}

type route struct {
	remoteID  string
	remoteUrl string
}

var (
	DisconnectdPacket = packets.NewControlPacket(packets.Disconnect).(*packets.DisconnectPacket)
	r                 = rand.New(rand.NewSource(time.Now().UnixNano()))
)

func (c *client) init() {
	c.status = Connected
	c.info.localIP = strings.Split(c.conn.LocalAddr().String(), ":")[0]
	c.info.remoteIP = strings.Split(c.conn.RemoteAddr().String(), ":")[0]
	c.ctx, c.cancelFunc = context.WithCancel(context.Background())
	c.subMap = make(map[string]*subscription)
	c.topicsMgr = c.broker.topicsMgr
}

func (c *client) readLoop() {
	nc := c.conn
	b := c.broker
	if nc == nil || b == nil {
		return
	}

	keepAlive := time.Second * time.Duration(c.info.keepalive)
	timeOut := keepAlive + (keepAlive / 2)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			//add read timeout
			if err := nc.SetReadDeadline(time.Now().Add(timeOut)); err != nil {
				log.Error("set read timeout error: ", zap.Error(err), zap.String("ClientID", c.info.clientID))
				msg := &Message{
					client: c,
					packet: DisconnectdPacket,
				}
				b.SubmitWork(c.info.clientID, msg)
				return
			}

			packet, err := packets.ReadPacket(nc)
			if err != nil {
				log.Error("read packet error: ", zap.Error(err), zap.String("ClientID", c.info.clientID))
				msg := &Message{
					client: c,
					packet: DisconnectdPacket,
				}
				b.SubmitWork(c.info.clientID, msg)
				return
			}

			msg := &Message{
				client: c,
				packet: packet,
			}
			b.SubmitWork(c.info.clientID, msg)
		}
	}

}

func ProcessMessage(msg *Message) {
	c := msg.client
	ca := msg.packet
	if ca == nil {
		return
	}

	if c.typ == CLIENT {
		log.Debug("Recv message:", zap.String("message type", reflect.TypeOf(msg.packet).String()[9:]), zap.String("ClientID", c.info.clientID))
	}

	switch ca.(type) {
	case *packets.ConnackPacket:
	case *packets.ConnectPacket:
	case *packets.PublishPacket:
		packet := ca.(*packets.PublishPacket)
		c.ProcessPublish(packet)
	case *packets.PubackPacket:
	case *packets.PubrecPacket:
	case *packets.PubrelPacket:
	case *packets.PubcompPacket:
	case *packets.SubscribePacket:
		packet := ca.(*packets.SubscribePacket)
		c.ProcessSubscribe(packet)
	case *packets.SubackPacket:
	case *packets.UnsubscribePacket:
		packet := ca.(*packets.UnsubscribePacket)
		c.ProcessUnSubscribe(packet)
	case *packets.UnsubackPacket:
	case *packets.PingreqPacket:
		c.ProcessPing()
	case *packets.PingrespPacket:
	case *packets.DisconnectPacket:
		c.Close()
	default:
		log.Info("Recv Unknow message.......", zap.String("ClientID", c.info.clientID))
	}
}

func (c *client) ProcessPublish(packet *packets.PublishPacket) {
	switch c.typ {
	case CLIENT:
		c.processClientPublish(packet)
	case ROUTER:
		c.processRemotePublish(packet)
	}

}

func (c *client) processRemotePublish(packet *packets.PublishPacket) {
	if c.status == Disconnected {
		return
	}

	topic := packet.TopicName
	if topic == BrokerInfoTopic {
		c.ProcessInfo(packet)
		return
	}

}

func (c *client) processClientPublish(packet *packets.PublishPacket) {
	if c.status == Disconnected {
		return
	}

	topic := packet.TopicName

	if !c.broker.CheckTopicAuth(PUB, c.info.username, topic) {
		log.Error("Pub Topics Auth failed, ", zap.String("topic", topic), zap.String("ClientID", c.info.clientID))
		return
	}

	//publish kafka
	c.broker.Publish(&bridge.Elements{
		ClientID:  c.info.clientID,
		Username:  c.info.username,
		Action:    bridge.Publish,
		Timestamp: time.Now().Unix(),
		Payload:   string(packet.Payload),
		Topic:     topic,
	})

	switch packet.Qos {
	case QosAtMostOnce:
		c.broker.PublishMessage(packet)
	case QosAtLeastOnce:
		puback := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
		puback.MessageID = packet.MessageID
		if err := c.WriterPacket(puback); err != nil {
			log.Error("send puback error, ", zap.Error(err), zap.String("ClientID", c.info.clientID))
			return
		}
		c.broker.PublishMessage(packet)
	case QosExactlyOnce:
		return
	default:
		log.Error("publish with unknown qos", zap.String("ClientID", c.info.clientID))
		return
	}

}

func (c *client) ProcessSubscribe(packet *packets.SubscribePacket) {
	switch c.typ {
	case CLIENT:
		c.processClientSubscribe(packet)
	}
}

func (c *client) processClientSubscribe(packet *packets.SubscribePacket) {
	if c.status == Disconnected {
		return
	}

	b := c.broker
	if b == nil {
		return
	}
	topics := packet.Topics
	qoss := packet.Qoss

	suback := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
	suback.MessageID = packet.MessageID
	var retcodes []byte

	for i, topic := range topics {
		t := topic
		//check topic auth for client
		if !b.CheckTopicAuth(SUB, c.info.username, topic) {
			log.Error("Sub topic Auth failed: ", zap.String("topic", topic), zap.String("ClientID", c.info.clientID))
			retcodes = append(retcodes, QosFailure)
			continue
		}

		b.Publish(&bridge.Elements{
			ClientID:  c.info.clientID,
			Username:  c.info.username,
			Action:    bridge.Subscribe,
			Timestamp: time.Now().Unix(),
			Topic:     topic,
		})

		groupName := ""
		share := false
		if strings.HasPrefix(topic, "$share/") {
			substr := groupCompile.FindStringSubmatch(topic)
			if len(substr) != 3 {
				retcodes = append(retcodes, QosFailure)
				continue
			}
			share = true
			groupName = substr[1]
			topic = substr[2]
		}

		sub := &subscription{
			topic:     topic,
			qos:       qoss[i],
			client:    c,
			share:     share,
			groupName: groupName,
		}

		rqos, err := c.topicsMgr.Subscribe([]byte(topic), qoss[i], sub)
		if err != nil {
			log.Error("subscribe error, ", zap.Error(err), zap.String("ClientID", c.info.clientID))
			retcodes = append(retcodes, QosFailure)
			continue
		}

		c.subMap[t] = sub

		c.session.AddTopic(t, qoss[i])
		retcodes = append(retcodes, rqos)
		c.topicsMgr.Retained([]byte(topic), &c.rmsgs)

	}

	suback.ReturnCodes = retcodes

	err := c.WriterPacket(suback)
	if err != nil {
		log.Error("send suback error, ", zap.Error(err), zap.String("ClientID", c.info.clientID))
		return
	}

	//process retain message
	for _, rm := range c.rmsgs {
		if err := c.WriterPacket(rm); err != nil {
			log.Error("Error publishing retained message:", zap.Any("err", err), zap.String("ClientID", c.info.clientID))
		} else {
			log.Info("process retain  message: ", zap.Any("packet", packet), zap.String("ClientID", c.info.clientID))
		}
	}
}

func (c *client) ProcessUnSubscribe(packet *packets.UnsubscribePacket) {
	switch c.typ {
	case CLIENT:
		c.processClientUnSubscribe(packet)
	}
}

func (c *client) processClientUnSubscribe(packet *packets.UnsubscribePacket) {
	if c.status == Disconnected {
		return
	}
	b := c.broker
	if b == nil {
		return
	}
	topics := packet.Topics

	for _, topic := range topics {
		{
			//publish kafka

			b.Publish(&bridge.Elements{
				ClientID:  c.info.clientID,
				Username:  c.info.username,
				Action:    bridge.Unsubscribe,
				Timestamp: time.Now().Unix(),
				Topic:     topic,
			})

		}

		sub, exist := c.subMap[topic]
		if exist {
			c.topicsMgr.Unsubscribe([]byte(sub.topic), sub)
			c.session.RemoveTopic(topic)
			delete(c.subMap, topic)
		}

	}

	unsuback := packets.NewControlPacket(packets.Unsuback).(*packets.UnsubackPacket)
	unsuback.MessageID = packet.MessageID

	err := c.WriterPacket(unsuback)
	if err != nil {
		log.Error("send unsuback error, ", zap.Error(err), zap.String("ClientID", c.info.clientID))
		return
	}
}

func (c *client) ProcessPing() {
	if c.status == Disconnected {
		return
	}
	resp := packets.NewControlPacket(packets.Pingresp).(*packets.PingrespPacket)
	err := c.WriterPacket(resp)
	if err != nil {
		log.Error("send PingResponse error, ", zap.Error(err), zap.String("ClientID", c.info.clientID))
		return
	}
}

func (c *client) Close() {
	if c.status == Disconnected {
		return
	}

	c.cancelFunc()

	c.status = Disconnected

	b := c.broker
	b.Publish(&bridge.Elements{
		ClientID:  c.info.clientID,
		Username:  c.info.username,
		Action:    bridge.Disconnect,
		Timestamp: time.Now().Unix(),
	})

	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	subs := c.subMap

	if b != nil {
		b.removeClient(c)
		for _, sub := range subs {
			err := b.topicsMgr.Unsubscribe([]byte(sub.topic), sub)
			if err != nil {
				log.Error("unsubscribe error, ", zap.Error(err), zap.String("ClientID", c.info.clientID))
			}
		}

		if c.typ == CLIENT {
			//offline notification
			b.OnlineOfflineNotification(c.info.clientID, false)
			if c.info.willMsg != nil {
				b.PublishMessage(c.info.willMsg)
			}
		}

		if c.typ == ROUTER {
			b.ConnectToDiscovery()
		}
	}
}

func (c *client) WriterPacket(packet packets.ControlPacket) error {
	if c.status == Disconnected {
		return nil
	}

	if packet == nil {
		return nil
	}
	if c.conn == nil {
		c.Close()
		return errors.New("connect lost ....")
	}

	c.mu.Lock()
	err := packet.Write(c.conn)
	c.mu.Unlock()
	return err
}
