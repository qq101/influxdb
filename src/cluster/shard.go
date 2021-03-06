package cluster

import (
	"common"
	"engine"
	"fmt"
	"parser"
	p "protocol"
	"sort"
	"strings"
	"time"
	"wal"

	log "code.google.com/p/log4go"
)

// A shard imements an interface for writing and querying data.
// It can be copied to multiple servers or the local datastore.
// Shard contains data from [startTime, endTime)
// Ids are unique across the cluster
type Shard interface {
	Id() uint32
	StartTime() time.Time
	EndTime() time.Time
	Write(*p.Request) error
	Query(querySpec *parser.QuerySpec, response chan *p.Response)
	IsMicrosecondInRange(t int64) bool
}

// Passed to a shard (local datastore or whatever) that gets yielded points from series.
type QueryProcessor interface {
	// This method returns true if the query should continue. If the query should be stopped,
	// like maybe the limit was hit, it should return false
	YieldPoint(seriesName *string, columnNames []string, point *p.Point) bool
	YieldSeries(seriesIncoming *p.Series) bool
	Close()

	// Set by the shard, so EXPLAIN query can know query against which shard is being measured
	SetShardInfo(shardId int, shardLocal bool)

	// Let QueryProcessor identify itself. What if it is a spy and we can't check that?
	GetName() string
}

type NewShardData struct {
	Id            uint32 `json:",omitempty"`
	StartTime     time.Time
	EndTime       time.Time
	ServerIds     []uint32
	Type          ShardType
	DurationSplit bool `json:",omitempty"`
}

type ShardType int

const (
	LONG_TERM ShardType = iota
	SHORT_TERM
)

type ShardData struct {
	id              uint32
	startTime       time.Time
	startMicro      int64
	endMicro        int64
	endTime         time.Time
	wal             WAL
	servers         []wal.Server
	clusterServers  []*ClusterServer
	store           LocalShardStore
	serverIds       []uint32
	shardType       ShardType
	durationIsSplit bool
	shardDuration   time.Duration
	shardSeconds    int64
	localServerId   uint32
	IsLocal         bool
}

func NewShard(id uint32, startTime, endTime time.Time, shardType ShardType, durationIsSplit bool, wal WAL) *ShardData {
	shardDuration := endTime.Sub(startTime)
	return &ShardData{
		id:              id,
		startTime:       startTime,
		endTime:         endTime,
		wal:             wal,
		startMicro:      common.TimeToMicroseconds(startTime),
		endMicro:        common.TimeToMicroseconds(endTime),
		serverIds:       make([]uint32, 0),
		shardType:       shardType,
		durationIsSplit: durationIsSplit,
		shardDuration:   shardDuration,
		shardSeconds:    int64(shardDuration.Seconds()),
	}
}

const (
	PER_SERVER_BUFFER_SIZE  = 10
	LOCAL_WRITE_BUFFER_SIZE = 10
)

var (
	queryResponse        = p.Response_QUERY
	endStreamResponse    = p.Response_END_STREAM
	accessDeniedResponse = p.Response_ACCESS_DENIED
	queryRequest         = p.Request_QUERY
	dropDatabaseRequest  = p.Request_DROP_DATABASE
)

type LocalShardDb interface {
	Write(database string, series *p.Series) error
	Query(*parser.QuerySpec, QueryProcessor) error
	DropDatabase(database string) error
	IsClosed() bool
}

type LocalShardStore interface {
	Write(request *p.Request) error
	SetWriteBuffer(writeBuffer *WriteBuffer)
	BufferWrite(request *p.Request)
	GetOrCreateShard(id uint32) (LocalShardDb, error)
	ReturnShard(id uint32)
	DeleteShard(shardId uint32) error
}

func (self *ShardData) Id() uint32 {
	return self.id
}

func (self *ShardData) StartMicro() int64 {
	return self.startMicro
}

func (self *ShardData) StartTime() time.Time {
	return self.startTime
}

func (self *ShardData) EndMicro() int64 {
	return self.endMicro
}

func (self *ShardData) EndTime() time.Time {
	return self.endTime
}

func (self *ShardData) IsMicrosecondInRange(t int64) bool {
	return t >= self.startMicro && t < self.endMicro
}

func (self *ShardData) SetServers(servers []*ClusterServer) {
	self.clusterServers = servers
	self.servers = make([]wal.Server, len(servers), len(servers))
	for i, server := range servers {
		self.serverIds = append(self.serverIds, server.Id)
		self.servers[i] = server
	}
	self.sortServerIds()
}

func (self *ShardData) SetLocalStore(store LocalShardStore, localServerId uint32) error {
	self.serverIds = append(self.serverIds, localServerId)
	self.localServerId = localServerId
	self.sortServerIds()

	self.store = store
	// make sure we can open up the shard
	_, err := self.store.GetOrCreateShard(self.id)
	if err != nil {
		return err
	}
	self.store.ReturnShard(self.id)
	self.IsLocal = true

	return nil
}

func (self *ShardData) ServerIds() []uint32 {
	return self.serverIds
}

func (self *ShardData) Write(request *p.Request) error {
	request.ShardId = &self.id
	requestNumber, err := self.wal.AssignSequenceNumbersAndLog(request, self)
	if err != nil {
		return err
	}
	request.RequestNumber = &requestNumber
	if self.store != nil {
		self.store.BufferWrite(request)
	}
	for _, server := range self.clusterServers {
		// we have to create a new reqeust object because the ID gets assigned on each server.
		requestWithoutId := &p.Request{Type: request.Type, Database: request.Database, Series: request.Series, ShardId: &self.id, RequestNumber: request.RequestNumber}
		server.BufferWrite(requestWithoutId)
	}
	return nil
}

func (self *ShardData) WriteLocalOnly(request *p.Request) error {
	self.store.Write(request)
	return nil
}

func (self *ShardData) Query(querySpec *parser.QuerySpec, response chan *p.Response) {
	// This is only for queries that are deletes or drops. They need to be sent everywhere as opposed to just the local or one of the remote shards.
	// But this boolean should only be set to true on the server that receives the initial query.
	if querySpec.RunAgainstAllServersInShard {
		if querySpec.IsDeleteFromSeriesQuery() {
			self.logAndHandleDeleteQuery(querySpec, response)
		} else if querySpec.IsDropSeriesQuery() {
			self.logAndHandleDropSeriesQuery(querySpec, response)
		}
	}

	if self.IsLocal {
		var processor QueryProcessor
		var err error

		if querySpec.IsListSeriesQuery() {
			processor = engine.NewListSeriesEngine(response)
		} else if querySpec.IsDeleteFromSeriesQuery() || querySpec.IsDropSeriesQuery() || querySpec.IsSinglePointQuery() {
			maxDeleteResults := 10000
			processor = engine.NewPassthroughEngine(response, maxDeleteResults)
		} else {
			query := querySpec.SelectQuery()
			if self.ShouldAggregateLocally(querySpec) {
				log.Debug("creating a query engine\n")
				processor, err = engine.NewQueryEngine(query, response)
				if err != nil {
					response <- &p.Response{Type: &endStreamResponse, ErrorMessage: p.String(err.Error())}
					log.Error("Error while creating engine: %s", err)
					return
				}
				processor.SetShardInfo(int(self.Id()), self.IsLocal)
			} else if query.HasAggregates() {
				maxPointsToBufferBeforeSending := 1000
				log.Debug("creating a passthrough engine\n")
				processor = engine.NewPassthroughEngine(response, maxPointsToBufferBeforeSending)
			} else {
				maxPointsToBufferBeforeSending := 1000
				log.Debug("creating a passthrough engine with limit\n")
				processor = engine.NewPassthroughEngineWithLimit(response, maxPointsToBufferBeforeSending, query.Limit)
			}
			processor = engine.NewFilteringEngine(query, processor)
		}
		shard, err := self.store.GetOrCreateShard(self.id)
		if err != nil {
			response <- &p.Response{Type: &endStreamResponse, ErrorMessage: p.String(err.Error())}
			log.Error("Error while getting shards: %s", err)
			return
		}
		defer self.store.ReturnShard(self.id)
		err = shard.Query(querySpec, processor)
		processor.Close()
		if err != nil {
			response <- &p.Response{Type: &endStreamResponse, ErrorMessage: p.String(err.Error())}
		}
		response <- &p.Response{Type: &endStreamResponse}
		return
	}

	healthyServers := make([]*ClusterServer, 0, len(self.clusterServers))
	for _, s := range self.clusterServers {
		if !s.IsUp() {
			continue
		}
		healthyServers = append(healthyServers, s)
	}
	healthyCount := len(healthyServers)
	if healthyCount == 0 {
		message := fmt.Sprintf("No servers up to query shard %d", self.id)
		response <- &p.Response{Type: &endStreamResponse, ErrorMessage: &message}
		log.Error(message)
		return
	}
	randServerIndex := int(time.Now().UnixNano() % int64(healthyCount))
	server := healthyServers[randServerIndex]
	request := self.createRequest(querySpec)

	server.MakeRequest(request, response)
}

func (self *ShardData) DropDatabase(database string, sendToServers bool) {
	if self.IsLocal {
		if shard, err := self.store.GetOrCreateShard(self.id); err == nil {
			defer self.store.ReturnShard(self.id)
			shard.DropDatabase(database)
		}
	}

	if !sendToServers {
		return
	}

	responses := make([]chan *p.Response, len(self.clusterServers), len(self.clusterServers))
	for i, server := range self.clusterServers {
		responseChan := make(chan *p.Response, 1)
		responses[i] = responseChan
		request := &p.Request{Type: &dropDatabaseRequest, Database: &database, ShardId: &self.id}
		go server.MakeRequest(request, responseChan)
	}
	for _, responseChan := range responses {
		// TODO: handle error responses
		<-responseChan
	}
}

func (self *ShardData) String() string {
	serversString := make([]string, 0)
	for _, s := range self.servers {
		serversString = append(serversString, fmt.Sprintf("%d", s.GetId()))
	}
	local := "false"
	if self.IsLocal {
		local = "true"
	}

	return fmt.Sprintf("[ID: %d, START: %d, END: %d, LOCAL: %s, SERVERS: [%s]]", self.id, self.startMicro, self.endMicro, local, strings.Join(serversString, ","))
}

func (self *ShardData) ShouldAggregateLocally(querySpec *parser.QuerySpec) bool {
	if self.durationIsSplit && querySpec.ReadsFromMultipleSeries() {
		return false
	}
	groupByInterval := querySpec.GetGroupByInterval()
	if groupByInterval == nil {
		if querySpec.HasAggregates() {
			return false
		}
		return true
	}
	if self.shardDuration%*groupByInterval == 0 {
		return true
	}
	return false
}

func (self *ShardData) QueryResponseBufferSize(querySpec *parser.QuerySpec, batchPointSize int) int {
	groupByTime := querySpec.GetGroupByInterval()
	if groupByTime == nil {
		// If the group by time is nil, we shouldn't have to use a buffer since the shards should be queried sequentially.
		// However, set this to something high just to be safe.
		log.Info("BUFFER SIZE: 1000")
		return 1000
	}
	tickCount := int(self.shardSeconds / int64(groupByTime.Seconds()))
	if tickCount < 10 {
		tickCount = 100
	} else if tickCount > 1000 {
		// cap this because each response should have up to this number of points in it.
		tickCount = tickCount / batchPointSize

		// but make sure it's at least 1k
		if tickCount < 1000 {
			tickCount = 1000
		}
	}
	columnCount := querySpec.GetGroupByColumnCount()
	if columnCount > 1 {
		// we don't really know the cardinality for any column up front. This is a just a multiplier so we'll see how this goes.
		// each response can have many points, so having a buffer of the ticks * 100 should be safe, but we'll see.
		tickCount = tickCount * 100
	}
	log.Info("BUFFER SIZE: ", tickCount)
	return tickCount
}

func (self *ShardData) logAndHandleDeleteQuery(querySpec *parser.QuerySpec, response chan *p.Response) {
	queryString := querySpec.GetQueryStringWithTimeCondition()
	request := self.createRequest(querySpec)
	request.Query = &queryString
	self.LogAndHandleDestructiveQuery(querySpec, request, response, false)
}

func (self *ShardData) logAndHandleDropSeriesQuery(querySpec *parser.QuerySpec, response chan *p.Response) {
	self.LogAndHandleDestructiveQuery(querySpec, self.createRequest(querySpec), response, false)
}

func (self *ShardData) LogAndHandleDestructiveQuery(querySpec *parser.QuerySpec, request *p.Request, response chan *p.Response, runLocalOnly bool) {
	if runLocalOnly {
		self.HandleDestructiveQuery(querySpec, request, response, true)
	}

	self.HandleDestructiveQuery(querySpec, request, response, false)
}

func (self *ShardData) deleteDataLocally(querySpec *parser.QuerySpec) (<-chan *p.Response, error) {
	localResponses := make(chan *p.Response, 1)

	// this doesn't really apply at this point since destructive queries don't output anything, but it may later
	maxPointsFromDestructiveQuery := 1000
	processor := engine.NewPassthroughEngine(localResponses, maxPointsFromDestructiveQuery)
	shard, err := self.store.GetOrCreateShard(self.id)
	if err != nil {
		return nil, err
	}
	defer self.store.ReturnShard(self.id)
	err = shard.Query(querySpec, processor)
	processor.Close()
	return localResponses, err
}

func (self *ShardData) forwardRequest(request *p.Request) ([]<-chan *p.Response, []uint32, error) {
	ids := []uint32{}
	responses := []<-chan *p.Response{}
	for _, server := range self.clusterServers {
		responseChan := make(chan *p.Response, 1)
		// do this so that a new id will get assigned
		request.Id = nil
		log.Debug("Forwarding request %s to %d", request.GetDescription(), server.Id)
		server.MakeRequest(request, responseChan)
		responses = append(responses, responseChan)
		ids = append(ids, server.Id)
	}
	return responses, ids, nil
}

func (self *ShardData) HandleDestructiveQuery(querySpec *parser.QuerySpec, request *p.Request, response chan *p.Response, runLocalOnly bool) {
	if !self.IsLocal && runLocalOnly {
		panic("WTF islocal is false and runLocalOnly is true")
	}

	responseCahnnels := []<-chan *p.Response{}
	serverIds := []uint32{}

	if self.IsLocal {
		channel, err := self.deleteDataLocally(querySpec)
		if err != nil {
			msg := err.Error()
			response <- &p.Response{Type: &endStreamResponse, ErrorMessage: &msg}
			log.Error(msg)
			return
		}
		responseCahnnels = append(responseCahnnels, channel)
		serverIds = append(serverIds, self.localServerId)
	}

	log.Debug("request %s, runLocalOnly: %v", request.GetDescription(), runLocalOnly)
	if !runLocalOnly {
		responses, ids, _ := self.forwardRequest(request)
		serverIds = append(serverIds, ids...)
		responseCahnnels = append(responseCahnnels, responses...)
	}

	accessDenied := false
	for idx, channel := range responseCahnnels {
		serverId := serverIds[idx]
		log.Debug("Waiting for response to %s from %d", request.GetDescription(), serverId)
		for {
			res := <-channel
			log.Debug("Received %s response from %d for %s", res.GetType(), serverId, request.GetDescription())
			if *res.Type == endStreamResponse {
				break
			}

			// don't send the access denied response until the end so the readers don't close out before the other responses.
			// See https://github.com/influxdb/influxdb/issues/316 for more info.
			if *res.Type != accessDeniedResponse {
				response <- res
			} else {
				accessDenied = true
			}
		}
	}

	if accessDenied {
		response <- &p.Response{Type: &accessDeniedResponse}
	}
	response <- &p.Response{Type: &endStreamResponse}
}

func (self *ShardData) createRequest(querySpec *parser.QuerySpec) *p.Request {
	queryString := querySpec.GetQueryString()
	user := querySpec.User()
	userName := user.GetName()
	database := querySpec.Database()
	isDbUser := !user.IsClusterAdmin()

	return &p.Request{
		Type:     &queryRequest,
		ShardId:  &self.id,
		Query:    &queryString,
		UserName: &userName,
		Database: &database,
		IsDbUser: &isDbUser,
	}
}

// used to serialize shards when sending around in raft or when snapshotting in the log
func (self *ShardData) ToNewShardData() *NewShardData {
	return &NewShardData{
		Id:        self.id,
		StartTime: self.startTime,
		EndTime:   self.endTime,
		Type:      self.shardType,
		ServerIds: self.serverIds,
	}
}

// server ids should always be returned in sorted order
func (self *ShardData) sortServerIds() {
	serverIdInts := make([]int, len(self.serverIds), len(self.serverIds))
	for i, id := range self.serverIds {
		serverIdInts[i] = int(id)
	}
	sort.Ints(serverIdInts)
	for i, id := range serverIdInts {
		self.serverIds[i] = uint32(id)
	}
}

func SortShardsByTimeAscending(shards []*ShardData) {
	sort.Sort(ByShardTimeAsc{shards})
}

func SortShardsByTimeDescending(shards []*ShardData) {
	sort.Sort(ByShardTimeDesc{shards})
}

type ShardCollection []*ShardData

func (s ShardCollection) Len() int      { return len(s) }
func (s ShardCollection) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

type ByShardTimeDesc struct{ ShardCollection }
type ByShardTimeAsc struct{ ShardCollection }

func (s ByShardTimeAsc) Less(i, j int) bool {
	if s.ShardCollection[i] != nil && s.ShardCollection[j] != nil {
		iStartTime := s.ShardCollection[i].StartTime().Unix()
		jStartTime := s.ShardCollection[j].StartTime().Unix()
		if iStartTime == jStartTime {
			return s.ShardCollection[i].Id() < s.ShardCollection[j].Id()
		}
		return iStartTime < jStartTime
	}
	return false
}
func (s ByShardTimeDesc) Less(i, j int) bool {
	if s.ShardCollection[i] != nil && s.ShardCollection[j] != nil {
		iStartTime := s.ShardCollection[i].StartTime().Unix()
		jStartTime := s.ShardCollection[j].StartTime().Unix()
		if iStartTime == jStartTime {
			return s.ShardCollection[i].Id() < s.ShardCollection[j].Id()
		}
		return iStartTime > jStartTime
	}
	return false
}
