package worker

import (
	"common/dlog"
	"common/g"
	"common/scheme"
	"common/utils"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"common/proc/metric"

	"github.com/parnurzeal/gorequest"
)

type OdinPoint struct {
	Name      string            `json:"name"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
	Tags      map[string]string `json:"tags"`
	Step      int64             `json:"step"`
	Offset    int64             `json:"offset"`
	NS        []string          `json:"-"`
}

type FalconPoint struct {
	Endpoint    string  `json:"endpoint"`
	Metric      string  `json:"metric"`
	Timestamp   int64   `json:"timestamp"`
	Step        int64   `json:"step"`
	Value       float64 `json:"value"`
	CounterType string  `json:"counterType"`
	Tags        string  `json:"tags"`
}

type SortByTms []*FalconPoint

func (p SortByTms) Len() int           { return len(p) }
func (p SortByTms) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p SortByTms) Less(i, j int) bool { return p[i].Timestamp < p[j].Timestamp }

var pushQueue chan *FalconPoint

func init() {
	//拍一个队列大小,10s一清
	pushQueue = make(chan *FalconPoint, 1024*100)
}

func PusherStart() {
	PosterLoop() //归类，批量发送给odin-agent
	PusherLoop() //计算，推送给发送队列
}

//循环推送，10s一次
func PosterLoop() {
	dlog.Info("PosterLoop Start")
	go func() {
		for {
			select {
			case p := <-pushQueue:
				points := make([]*FalconPoint, 0)
				points = append(points, p)
			DONE:
				for {
					select {
					case tmp := <-pushQueue:
						points = append(points, tmp)
						continue
					default:
						break DONE
					}
				}
				//先推到cache中
				PostToCache(points)
				//开一个协程，异步发送至odin-agent
				go postToFalconAgent(points)
			}
			time.Sleep(10 * time.Second)
		}
	}()
}

func PusherLoop() {
	dlog.Info("PushLoop Start")
	for {
		gIds := GlobalCount.GetIDs()
		for _, id := range gIds {
			stCount, err := GlobalCount.GetStrategyCountByID(id)
			step := stCount.Strategy.Interval

			filePath := stCount.Strategy.FilePath
			if err != nil {
				dlog.Errorf("get strategy count by id error : %v", err)
				continue
			}
			tmsList := stCount.GetTmsList()
			for _, tms := range tmsList {
				if tmsNeedPush(tms, filePath, step) {
					pointsCount, err := stCount.GetByTms(tms)
					if err == nil {
						pointMap := BeforePushQueue(stCount.Strategy, pointsCount.TagstringMap)
						ToPushQueue(stCount.Strategy, tms, pointMap)
					} else {
						dlog.Errorf("get by tms [%d] error : %v", tms, err)
					}
					stCount.DeleteTms(tms)
				}
			}
		}
		time.Sleep(time.Second * time.Duration(g.Conf().Worker.PushInterval))
	}
}

func tmsNeedPush(tms int64, filePath string, step int64) bool {
	readerOldestTms, exists := GetOldestTms(filePath)
	if !exists {
		return true
	}
	if tms < AlignStepTms(step, readerOldestTms) {
		return true
	}
	return false
}

//推数据之前
func BeforePushQueue(strategy *scheme.Strategy, pointMap map[string]*PointCounter) map[string]*PointCounter {
	beforePush := strategy.BeforePush
	if beforePush == "" {
		return pointMap
	}

	//配置了数据处理，则进行数据处理
	var ret map[string]*PointCounter
	switch strategy.BeforePush {
	case "max_count":
		ret = GetMaxCountPoint(pointMap)
	default:
		ret = pointMap
	}

	return ret
}

func GetMaxCountPoint(pointMap map[string]*PointCounter) map[string]*PointCounter {
	var ret map[string]*PointCounter
	var tmpPointCount int64 = 0

	for _, point := range pointMap {
		if point.Count > tmpPointCount {
			ret = make(map[string]*PointCounter, 0)
			ret["null"] = point //set tag key
			tmpPointCount = point.Count
		}
	}

	return ret
}

// 这个参数是为了最大限度的对接
// pointMap的key，是打平了的tagkv
func ToPushQueue(strategy *scheme.Strategy, tms int64, pointMap map[string]*PointCounter) error {
	for tagstring, PointCounter := range pointMap {
		var value float64 = 0
		switch strategy.Func {
		case "cnt":
			value = float64(PointCounter.Count)
		case "avg":
			if PointCounter.Count == 0 {
				//这种就不用往监控推了
				continue
			} else {
				avg := PointCounter.Sum / float64(PointCounter.Count)
				value = getPrecision(avg, strategy.Degree)
			}
		case "sum":
			value = PointCounter.Sum
		case "max":
			value = PointCounter.Max
		case "min":
			value = PointCounter.Min
		default:
			dlog.Error("Strategy Func Error: %s ", strategy.Func)
			return fmt.Errorf("Strategy Func Error: %s ", strategy.Func)
		}

		var tags map[string]string
		if tagstring == "null" {
			tags = make(map[string]string, 0)
		} else {
			tags = utils.DictedTagstring(tagstring)
		}

		hostname, err := utils.LocalHostname()
		if err != nil {
			dlog.Errorf("cannot get hostname : %v", err)
			return err
		}

		if math.IsNaN(value) {
			continue
		}

		tmpPoint := &FalconPoint{
			Endpoint:    hostname,
			Metric:      strategy.Name,
			Timestamp:   tms,
			Step:        strategy.Interval,
			Value:       value,
			Tags:        utils.SortedTags(tags),
			CounterType: "GAUGE",
		}
		pushQueue <- tmpPoint
	}

	return nil
}

func postToFalconAgent(paramPoints []*FalconPoint) {

	sort.Sort(SortByTms(paramPoints))

	param, err := json.Marshal(&paramPoints)

	start := time.Now()
	num := int64(len(paramPoints))

	if err != nil {
		dlog.Errorf("sent to falcon agent error : %s", err.Error())
		return
	}

	dlog.Infof("to falcon agent: %s", string(param))

	url := fmt.Sprintf(g.Conf().Worker.PushURL)

	resp, body, errs := gorequest.New().Post(url).
		Timeout(10 * time.Second).
		Send(string(param)).
		End()

	metric.MetricPushLatency(int64(time.Now().Sub(start) / time.Second))

	if errs != nil {
		dlog.Errorf("Post to falcon agent Request err : %s", errs)
		metric.MetricPushCnt(num, false)
		return
	}

	if resp.StatusCode != 200 {
		dlog.Errorf("Post to falcon agent Failed! [code:%d][body:%s]", resp.StatusCode, body)
		metric.MetricPushCnt(num, false)
		return
	} else {
		metric.MetricPushCnt(num, true)
		dlog.Infof("Post to falcon agent success! [code:%d][body:%s]", resp.StatusCode, body)
		return
	}
}

func getPrecision(num float64, degree int64) float64 {
	tmpFloat := num * float64(math.Pow10(int(degree)))
	tmpInt := int(tmpFloat + 0.5)
	return float64(tmpInt) / float64(math.Pow10(int(degree)))
}
