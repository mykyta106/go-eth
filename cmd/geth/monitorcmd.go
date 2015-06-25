package main

import (
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/codegangsta/cli"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/rpc/codec"
	"github.com/ethereum/go-ethereum/rpc/comms"
	"github.com/gizak/termui"
)

var (
	monitorCommandAttachFlag = cli.StringFlag{
		Name:  "attach",
		Value: "ipc:" + common.DefaultIpcPath(),
		Usage: "IPC or RPC API endpoint to attach to",
	}
	monitorCommandRowsFlag = cli.IntFlag{
		Name:  "rows",
		Value: 5,
		Usage: "Rows (maximum) to display the charts in",
	}
	monitorCommand = cli.Command{
		Action: monitor,
		Name:   "monitor",
		Usage:  `Geth Monitor: node metrics monitoring and visualization`,
		Description: `
The Geth monitor is a tool to collect and visualize various internal metrics
gathered by the node, supporting different chart types as well as the capacity
to display multiple metrics simultaneously.
`,
		Flags: []cli.Flag{
			monitorCommandAttachFlag,
			monitorCommandRowsFlag,
		},
	}
)

// monitor starts a terminal UI based monitoring tool for the requested metrics.
func monitor(ctx *cli.Context) {
	var (
		client comms.EthereumClient
		err    error
	)
	// Attach to an Ethereum node over IPC or RPC
	endpoint := ctx.String(monitorCommandAttachFlag.Name)
	if client, err = comms.ClientFromEndpoint(endpoint, codec.JSON); err != nil {
		utils.Fatalf("Unable to attach to geth node: %v", err)
	}
	defer client.Close()

	xeth := rpc.NewXeth(client)

	// Retrieve all the available metrics and resolve the user pattens
	metrics, err := retrieveMetrics(xeth)
	if err != nil {
		utils.Fatalf("Failed to retrieve system metrics: %v", err)
	}
	monitored := resolveMetrics(metrics, ctx.Args())
	sort.Strings(monitored)

	// Create the access function and check that the metric exists
	value := func(metrics map[string]interface{}, metric string) float64 {
		parts, found := strings.Split(metric, "/"), true
		for _, part := range parts[:len(parts)-1] {
			metrics, found = metrics[part].(map[string]interface{})
			if !found {
				utils.Fatalf("Metric not found: %s", metric)
			}
		}
		if v, ok := metrics[parts[len(parts)-1]].(float64); ok {
			return v
		}
		utils.Fatalf("Metric not float64: %s", metric)
		return 0
	}
	// Create and configure the chart UI defaults
	if err := termui.Init(); err != nil {
		utils.Fatalf("Unable to initialize terminal UI: %v", err)
	}
	defer termui.Close()

	termui.UseTheme("helloworld")

	rows := len(monitored)
	if max := ctx.Int(monitorCommandRowsFlag.Name); rows > max {
		rows = max
	}
	cols := (len(monitored) + rows - 1) / rows
	for i := 0; i < rows; i++ {
		termui.Body.AddRows(termui.NewRow())
	}
	// Create each individual data chart
	charts := make([]*termui.LineChart, len(monitored))
	data := make([][]float64, len(monitored))
	for i := 0; i < len(data); i++ {
		data[i] = make([]float64, 512)
	}
	for i, metric := range monitored {
		charts[i] = termui.NewLineChart()

		charts[i].Data = make([]float64, 512)
		charts[i].DataLabels = []string{""}
		charts[i].Height = termui.TermHeight() / rows
		charts[i].AxesColor = termui.ColorWhite
		charts[i].LineColor = termui.ColorGreen
		charts[i].PaddingBottom = -1

		charts[i].Border.Label = metric
		charts[i].Border.LabelFgColor = charts[i].Border.FgColor
		charts[i].Border.FgColor = charts[i].Border.BgColor

		row := termui.Body.Rows[i%rows]
		row.Cols = append(row.Cols, termui.NewCol(12/cols, 0, charts[i]))
	}
	termui.Body.Align()
	termui.Render(termui.Body)

	refresh := time.Tick(time.Second)
	for {
		select {
		case event := <-termui.EventCh():
			if event.Type == termui.EventKey && event.Ch == 'q' {
				return
			}
			if event.Type == termui.EventResize {
				termui.Body.Width = termui.TermWidth()
				for _, chart := range charts {
					chart.Height = termui.TermHeight() / rows
				}
				termui.Body.Align()
				termui.Render(termui.Body)
			}
		case <-refresh:
			metrics, err := retrieveMetrics(xeth)
			if err != nil {
				utils.Fatalf("Failed to retrieve system metrics: %v", err)
			}
			for i, metric := range monitored {
				data[i] = append([]float64{value(metrics, metric)}, data[i][:len(data[i])-1]...)
				updateChart(metric, data[i], charts[i])
			}
			termui.Render(termui.Body)
		}
	}
}

// retrieveMetrics contacts the attached geth node and retrieves the entire set
// of collected system metrics.
func retrieveMetrics(xeth *rpc.Xeth) (map[string]interface{}, error) {
	return xeth.Call("debug_metrics", []interface{}{true})
}

// resolveMetrics takes a list of input metric patterns, and resolves each to one
// or more canonical metric names.
func resolveMetrics(metrics map[string]interface{}, patterns []string) []string {
	res := []string{}
	for _, pattern := range patterns {
		res = append(res, resolveMetric(metrics, pattern, "")...)
	}
	return res
}

// resolveMetrics takes a single of input metric pattern, and resolves it to one
// or more canonical metric names.
func resolveMetric(metrics map[string]interface{}, pattern string, path string) []string {
	results := []string{}

	// If a nested metric was requested, recurse optionally branching (via comma)
	parts := strings.SplitN(pattern, "/", 2)
	if len(parts) > 1 {
		for _, variation := range strings.Split(parts[0], ",") {
			if submetrics, ok := metrics[variation].(map[string]interface{}); !ok {
				utils.Fatalf("Failed to retrieve system metrics: %s", path+variation)
				return nil
			} else {
				results = append(results, resolveMetric(submetrics, parts[1], path+variation+"/")...)
			}
		}
		return results
	}
	// Depending what the last link is, return or expand
	for _, variation := range strings.Split(pattern, ",") {
		switch metric := metrics[variation].(type) {
		case float64:
			// Final metric value found, return as singleton
			results = append(results, path+variation)

		case map[string]interface{}:
			results = append(results, expandMetrics(metric, path+variation+"/")...)

		default:
			utils.Fatalf("Metric pattern resolved to unexpected type: %v", reflect.TypeOf(metric))
			return nil
		}
	}
	return results
}

// expandMetrics expands the entire tree of metrics into a flat list of paths.
func expandMetrics(metrics map[string]interface{}, path string) []string {
	// Iterate over all fields and expand individually
	list := []string{}
	for name, metric := range metrics {
		switch metric := metric.(type) {
		case float64:
			// Final metric value found, append to list
			list = append(list, path+name)

		case map[string]interface{}:
			// Tree of metrics found, expand recursively
			list = append(list, expandMetrics(metric, path+name+"/")...)

		default:
			utils.Fatalf("Metric pattern %s resolved to unexpected type: %v", path+name, reflect.TypeOf(metric))
			return nil
		}
	}
	return list
}

// updateChart inserts a dataset into a line chart, scaling appropriately as to
// not display weird labels, also updating the chart label accordingly.
func updateChart(metric string, data []float64, chart *termui.LineChart) {
	dataUnits := []string{"", "K", "M", "G", "T", "E"}
	timeUnits := []string{"ns", "µs", "ms", "s", "ks", "ms"}
	colors := []termui.Attribute{termui.ColorBlue, termui.ColorCyan, termui.ColorGreen, termui.ColorYellow, termui.ColorRed, termui.ColorRed}

	// Find the maximum value and scale under 1K
	high := data[0]
	for _, value := range data[1:] {
		high = math.Max(high, value)
	}
	unit, scale := 0, 1.0
	for high >= 1000 {
		high, unit, scale = high/1000, unit+1, scale*1000
	}
	// Update the chart's data points with the scaled values
	for i, value := range data {
		chart.Data[i] = value / scale
	}
	// Update the chart's label with the scale units
	chart.Border.Label = metric

	units := dataUnits
	if strings.Contains(metric, "Percentiles") {
		units = timeUnits
	}
	if len(units[unit]) > 0 {
		chart.Border.Label += " [" + units[unit] + "]"
	}
	chart.LineColor = colors[unit]
}
