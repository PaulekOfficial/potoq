package potoq

import (
	"fmt"
	"io"
	"time"

	"github.com/Craftserve/potoq/packets"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

type HandlerCommand interface {
	Execute(handler *Handler) error
}

type HandlerCommandChan chan HandlerCommand

// >> ReconnectCommand -> causes handler to reconnect to another upstream and fake client world change

type ReconnectCommand struct {
	Name string
	Addr string
}

func (cmd *ReconnectCommand) Execute(handler *Handler) (err error) {
	if handler.UpstreamC != nil {
		handler.UpstreamTomb.Kill(nil)
		err = handler.UpstreamC.Close()
		if err != nil {
			return
		}
		handler.UpstreamC = nil
		handler.UpstreamW = nil
	}

	// handler.upstream_tomb.Wait() // wait for read_packets end
	// if handler.ClientSettings == nil { // ???
	// return fmt.Errorf("Reconnect error: ClientSettings is nil! %#v", handler.ClientSettings)
	// }

	err = handler.connectUpstream(cmd.Name, cmd.Addr)
	if err != nil { // TODO: brzydkie bledy beda jak sektor pelny chyba
		handler.Log().
			WithField("name", cmd.Name).
			WithField("address", cmd.Addr).
			WithError(err).
			Errorln("Connect to upstream failed")
		_ = handler.DownstreamW.WritePacket(packets.NewIngameKickTxt(err.Error()), true)
		return
	}

	// hijack Join Game packet
	var p packets.Packet
	select {
	case <-handler.UpstreamTomb.Dead():
		return handler.UpstreamTomb.Err()
	case p = <-handler.UpstreamPackets:
	}

	join, ok := p.(*packets.JoinGamePacketCB)
	if !ok {
		err = fmt.Errorf("Packet other than JoinGamePacketCB while reconnecting upstream: %#v", p)
		return
	}
	handler.Log().WithFields(logrus.Fields{
		"join": join,
	}).Debug("Reconnect Join")

	// used for scoreboard
	if derr := handler.dispatchPacket(join); derr != nil {
		return fmt.Errorf("Error in JoinGamePacketCB packet hook: %s", err)
	}

	// packets.WritePacket(handler.Upstream, handler.ClientSettings, true, handler.compress_threshold)
	// if handler.MCBrand != nil { // mc brand is not always catched
	// 	packets.WritePacket(handler.Upstream, handler.MCBrand, false, handler.compress_threshold)
	// } else {
	// 	handler.Log.Warn("minecraft:brand is nil for %s", handler)
	// }

	return SendDimensionSwitch(handler, join)

	// drop all waiting packets from client until
	// loop:
	// 	for {
	// 		p = <-handler.downstream_packets
	// 		if p == nil { // client conn closed
	// 			handler.upstream_tomb.Wait()
	// 			return handler.upstream_tomb.Err()
	// 		}
	// 		if handler.PacketTrace != nil {
	// 			_, err = io.WriteString(handler.PacketTrace, "reconnect: "+packets.ToString(p, packets.ServerBound)+"\n")
	// 			if err != nil {
	// 				return
	// 			}
	// 		}
	// 		// TODO: przerobic sprawdzenie ponizej na (&packets.PlayerPositionLookPacket{}).PacketId() - trzeba zaimplementowac tego structa
	// 		switch p.PacketID() {
	// 		case 0x10, 0x11, 0x04: // 0x10 Player Position, 0x11 Player Position And Look, 0x04 Client Settings //, 0x12, 0x1A:
	// 			packets.WritePacket(handler.Upstream, p, true, handler.compress_threshold)
	// 			handler.Log.Info("breaking reconnect")
	// 			break loop
	// 		}
	// 	}
	// return
}

func SendDimensionSwitch(handler *Handler, join *packets.JoinGamePacketCB) (err error) {
	w := handler.DownstreamW

	// Delete all tablist cells from
	currentPlayerList := handler.playerList
	if currentPlayerList != nil {
		values := make([]packets.PlayerListItem, 0, len(currentPlayerList))
		for _, v := range currentPlayerList {
			values = append(values, v)
		}

		clearPlayerListPacket := &packets.PlayerListItemPacketCB{
			Action: packets.REMOVE_PLAYER,
			Items:  values,
		}
		err = w.WritePacket(clearPlayerListPacket, false)
		if err != nil {
			return
		}
		handler.playerList = make(map[uuid.UUID]packets.PlayerListItem, 0)
	}

	// Delete header and footer
	err = w.WritePacket(&packets.PlayerListTitlePacketCB{
		Header: `{"text":""}`,
		Footer: `{"text":""}`,
	}, false)
	if err != nil {
		return
	}

	// send world change to client
	tempDim := &packets.RespawnPacketCB{
		Dimension:        join.Dimension,
		DimensionId:      join.DimensionId,
		GameMode:         join.GameMode,
		PreviousGameMode: join.PreviousGameMode,
		HashedSeed:       join.HashedSeed,
		IsDebug:          join.IsDebug,
		IsFlat:           join.IsFlat,
		CopyMetadata:     false,
	}
	if join.DimensionId == "minecraft:overworld" {
		tempDim.DimensionId = "minecraft:the_end"
	}
	err = w.WritePacket(tempDim, false)
	if err != nil {
		return
	}
	err = w.WritePacket(join, false)
	if err != nil {
		return
	}

	err = w.WritePacket(&packets.RespawnPacketCB{
		Dimension:        join.Dimension,
		DimensionId:      join.DimensionId,
		GameMode:         join.GameMode,
		PreviousGameMode: join.PreviousGameMode,
		HashedSeed:       join.HashedSeed,
		IsDebug:          join.IsDebug,
		IsFlat:           join.IsFlat,
		CopyMetadata:     false,
	}, false)
	if err != nil {
		return
	}

	err = w.WritePacket(&packets.GameStateChangePacketCB{
		Reason: 3,
		Value:  float32(join.GameMode),
	}, false)
	if err != nil {
		return
	}

	return w.Flush()
}

// InjectPacketCommand -> injects packet to selected side of connection

type injectPacketCommand struct {
	Direction packets.Direction
	Payload   []packets.Packet
	RaiseErr  error
}

func (cmd *injectPacketCommand) Execute(handler *Handler) (err error) {
	var w packets.PacketWriter
	switch cmd.Direction {
	case packets.ServerBound:
		w = handler.UpstreamW
	case packets.ClientBound:
		w = handler.DownstreamW
	default:
		panic(fmt.Sprintf("Unknown direction: %X", cmd.Direction))
	}
	if cmd.Payload == nil {
		panic("Nil payload!")
	}
	for _, packet := range cmd.Payload {
		if handler.PacketTrace != nil {
			_, err = io.WriteString(handler.PacketTrace, "inject: "+packets.ToString(packet, cmd.Direction)+"\n")
			if err != nil {
				return
			}
		}
		w.WritePacket(packet, false)
		if err != nil {
			return err
		}
	}

	err = w.Flush()
	if err != nil {
		return err
	}

	if cmd.RaiseErr == io.EOF {
		// wait for packet to be received by client... a little bit ugly.
		time.Sleep(time.Second)
	}
	return cmd.RaiseErr
}
