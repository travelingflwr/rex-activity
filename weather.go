package main

// Weather: the conditions where you are, from Open-Meteo (free, no key).
// A terminal program can't ask macOS for its location, so you name your
// place once (`rex-activity weather Phoenix`) and Open-Meteo's geocoder
// turns it into coordinates.

import (
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type weatherPlace struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
}

type config struct {
	Weather *weatherPlace `json:"weather,omitempty"`
	Units   string        `json:"units,omitempty"` // "f" or "c"; empty follows the system
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "rex-activity", "config.json")
}

func loadConfig() config {
	var c config
	if data, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(data, &c)
	}
	return c
}

func saveConfig(c config) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	return writeAtomic(configPath(), append(data, '\n'))
}

// fahrenheit is whether temperatures read in °F: the config's choice, else
// the system's.
func fahrenheit(c config) bool {
	switch strings.ToLower(c.Units) {
	case "f", "fahrenheit":
		return true
	case "c", "celsius":
		return false
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("/usr/bin/defaults", "read", "-g", "AppleTemperatureUnit").Output(); err == nil {
			return strings.TrimSpace(string(out)) == "Fahrenheit"
		}
		if out, err := exec.Command("/usr/bin/defaults", "read", "-g", "AppleLocale").Output(); err == nil {
			return usLocale(string(out))
		}
	}
	return usLocale(os.Getenv("LC_ALL") + os.Getenv("LC_MEASUREMENT") + os.Getenv("LANG"))
}

func usLocale(s string) bool {
	for _, r := range []string{"_US", "_LR", "_MM"} {
		if strings.Contains(s, r) {
			return true
		}
	}
	return false
}

// runWeather is `rex-activity weather [PLACE | --off | --units f|c]`.
func runWeather(args []string) error {
	c := loadConfig()
	switch {
	case len(args) == 0:
		if c.Weather == nil {
			fmt.Println("No place set. Try: rex-activity weather \"Portland, Oregon\"")
		} else {
			fmt.Printf("%s (%.3f, %.3f)\n", c.Weather.Name, c.Weather.Lat, c.Weather.Lon)
		}
		return nil
	case args[0] == "--off":
		c.Weather = nil
		return saveConfig(c)
	case args[0] == "--units" && len(args) == 2:
		c.Units = strings.ToLower(args[1])
		return saveConfig(c)
	}
	place, err := geocode(strings.Join(args, " "))
	if err != nil {
		return err
	}
	c.Weather = place
	if err := saveConfig(c); err != nil {
		return err
	}
	fmt.Printf("Weather for %s (%.3f, %.3f)\n", place.Name, place.Lat, place.Lon)
	return nil
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// geocode finds a place by name. "Portland, Oregon" looks up Portland and
// prefers the match whose region or country fits the rest.
func geocode(query string) (*weatherPlace, error) {
	name, hint, _ := strings.Cut(query, ",")
	name, hint = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(hint))
	u := "https://geocoding-api.open-meteo.com/v1/search?count=10&language=en&format=json&name=" + url.QueryEscape(name)
	resp, err := httpClient.Get(u)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach the place finder: %w", err)
	}
	defer resp.Body.Close()
	var res struct {
		Results []struct {
			Name    string  `json:"name"`
			Lat     float64 `json:"latitude"`
			Lon     float64 `json:"longitude"`
			Admin1  string  `json:"admin1"`
			Country string  `json:"country"`
			Code    string  `json:"country_code"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || len(res.Results) == 0 {
		return nil, fmt.Errorf("no place called %q", name)
	}
	pick := res.Results[0]
	if hint != "" {
		for _, r := range res.Results {
			for _, s := range []string{r.Admin1, r.Country, r.Code} {
				if s != "" && strings.HasPrefix(strings.ToLower(s), hint) {
					pick = r
					goto found
				}
			}
		}
	}
found:
	label := pick.Name
	if pick.Admin1 != "" && pick.Admin1 != pick.Name {
		label += ", " + pick.Admin1
	}
	return &weatherPlace{Name: label, Lat: pick.Lat, Lon: pick.Lon}, nil
}

type hourly struct {
	At   time.Time
	Temp float64
	Code int
	Day  bool
	Rain int
}

type conditions struct {
	Place                  string
	Temp, Feels, High, Low float64
	Code                   int
	Day                    bool
	Wind                   float64
	Humidity, Rain         int
	Hours                  []hourly
	F                      bool
	Fetched                time.Time
}

// weatherWatch keeps the conditions fresh, every 15 minutes.
type weatherWatch struct {
	mu    sync.Mutex
	cur   *conditions
	err   string
	place *weatherPlace
	wake  func()
	again chan struct{}
}

func newWeatherWatch(wake func()) *weatherWatch {
	w := &weatherWatch{wake: wake, again: make(chan struct{}, 1)}
	keepGoing("weather", w.run)
	return w
}

func (w *weatherWatch) refresh() {
	select {
	case w.again <- struct{}{}:
	default:
	}
}

func (w *weatherWatch) state() (*conditions, string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cur, w.err, w.place != nil
}

func (w *weatherWatch) run() {
	for {
		c := loadConfig()
		w.mu.Lock()
		w.place = c.Weather
		w.mu.Unlock()
		if c.Weather != nil {
			cur, err := fetchWeather(*c.Weather, fahrenheit(c))
			w.mu.Lock()
			if err == nil {
				w.cur, w.err = cur, ""
			} else if w.cur == nil || time.Since(w.cur.Fetched) > 2*time.Hour {
				w.err = "Couldn't reach the weather service"
			}
			w.mu.Unlock()
		}
		w.wake()
		stamp := fileStamp(configPath())
		until := time.Now().Add(15 * time.Minute)
	wait:
		for time.Now().Before(until) {
			select {
			case <-w.again:
				break wait
			case <-time.After(3 * time.Second):
				if fileStamp(configPath()) != stamp {
					break wait // a new place, or units
				}
			}
		}
	}
}

func fetchWeather(p weatherPlace, f bool) (*conditions, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.3f", p.Lat))
	q.Set("longitude", fmt.Sprintf("%.3f", p.Lon))
	q.Set("current", "temperature_2m,apparent_temperature,weather_code,is_day,wind_speed_10m,relative_humidity_2m")
	q.Set("hourly", "temperature_2m,weather_code,is_day,precipitation_probability")
	q.Set("daily", "temperature_2m_max,temperature_2m_min,precipitation_probability_max")
	q.Set("forecast_days", "2")
	q.Set("timezone", "auto")
	q.Set("timeformat", "unixtime")
	if f {
		q.Set("temperature_unit", "fahrenheit")
		q.Set("wind_speed_unit", "mph")
	}
	resp, err := httpClient.Get("https://api.open-meteo.com/v1/forecast?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Current struct {
			Temp     float64 `json:"temperature_2m"`
			Feels    float64 `json:"apparent_temperature"`
			Code     int     `json:"weather_code"`
			Day      int     `json:"is_day"`
			Wind     float64 `json:"wind_speed_10m"`
			Humidity int     `json:"relative_humidity_2m"`
		} `json:"current"`
		Hourly struct {
			Time []int64   `json:"time"`
			Temp []float64 `json:"temperature_2m"`
			Code []int     `json:"weather_code"`
			Day  []int     `json:"is_day"`
			Rain []*int    `json:"precipitation_probability"`
		} `json:"hourly"`
		Daily struct {
			High []float64 `json:"temperature_2m_max"`
			Low  []float64 `json:"temperature_2m_min"`
			Rain []*int    `json:"precipitation_probability_max"`
		} `json:"daily"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if len(r.Daily.High) == 0 || len(r.Daily.Low) == 0 {
		return nil, fmt.Errorf("no forecast")
	}
	c := &conditions{
		Place: p.Name, Temp: r.Current.Temp, Feels: r.Current.Feels, Code: r.Current.Code, Day: r.Current.Day == 1,
		Wind: r.Current.Wind, Humidity: r.Current.Humidity, High: r.Daily.High[0], Low: r.Daily.Low[0],
		F: f, Fetched: time.Now(),
	}
	if len(r.Daily.Rain) > 0 && r.Daily.Rain[0] != nil {
		c.Rain = *r.Daily.Rain[0]
	}
	// The next hours, every other one.
	now := time.Now()
	h := r.Hourly
	for i := range h.Time {
		t := time.Unix(h.Time[i], 0)
		if t.Before(now.Truncate(time.Hour).Add(time.Hour)) || i >= len(h.Temp) || i >= len(h.Code) {
			continue
		}
		hr := hourly{At: t, Temp: h.Temp[i], Code: h.Code[i]}
		if i < len(h.Day) {
			hr.Day = h.Day[i] == 1
		}
		if i < len(h.Rain) && h.Rain[i] != nil {
			hr.Rain = *h.Rain[i]
		}
		c.Hours = append(c.Hours, hr)
	}
	var every []hourly
	for i := 0; i < len(c.Hours) && len(every) < 6; i += 2 {
		every = append(every, c.Hours[i])
	}
	c.Hours = every
	return c, nil
}

// weatherText names a WMO weather code.
func weatherText(code int) string {
	switch code {
	case 0:
		return "Clear"
	case 1:
		return "Mostly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Overcast"
	case 45, 48:
		return "Fog"
	case 51, 53, 55:
		return "Drizzle"
	case 56, 57:
		return "Freezing drizzle"
	case 61:
		return "Light rain"
	case 63:
		return "Rain"
	case 65:
		return "Heavy rain"
	case 66, 67:
		return "Freezing rain"
	case 71:
		return "Light snow"
	case 73:
		return "Snow"
	case 75:
		return "Heavy snow"
	case 77:
		return "Snow grains"
	case 80, 81:
		return "Rain showers"
	case 82:
		return "Heavy showers"
	case 85, 86:
		return "Snow showers"
	case 95:
		return "Thunderstorm"
	case 96, 99:
		return "Thunderstorm, hail"
	}
	return "—"
}

// --- the weather icons, drawn -----------------------------------------------

type skyKind int

const (
	skyClear skyKind = iota
	skyPartly
	skyCloud
	skyFog
	skyRain
	skySnow
	skyStorm
)

func skyOf(code int) skyKind {
	switch {
	case code <= 1:
		return skyClear
	case code == 2:
		return skyPartly
	case code == 3:
		return skyCloud
	case code == 45 || code == 48:
		return skyFog
	case code >= 71 && code <= 77, code == 85, code == 86:
		return skySnow
	case code >= 95:
		return skyStorm
	}
	return skyRain
}

type skyColors struct{ sun, moon, cloud, cloudDark, rain, snow, bolt color.RGBA }

func skyPalette(r *renderer) skyColors {
	return skyColors{
		sun:       r.themed(rgb(240, 160, 20), rgb(255, 196, 70)),
		moon:      r.themed(rgb(150, 160, 190), rgb(214, 220, 236)),
		cloud:     r.themed(rgb(176, 184, 198), rgb(206, 212, 224)),
		cloudDark: r.themed(rgb(132, 140, 156), rgb(150, 158, 174)),
		rain:      r.themed(rgb(46, 120, 230), rgb(110, 170, 255)),
		snow:      r.themed(rgb(120, 160, 210), rgb(230, 238, 250)),
		bolt:      r.themed(rgb(230, 170, 0), rgb(255, 210, 70)),
	}
}

// drawSky draws the icon for a weather code in a size×size box at (x, y).
func drawSky(p avPaint, code int, day bool, x, y, size float64) {
	if !p.on() {
		return
	}
	r := p.r
	sc := skyPalette(r)
	u := size / 48 // the icon is designed on a 48-point grid
	kind := skyOf(code)
	cloudCol := sc.cloud
	if kind == skyRain || kind == skyStorm || kind == skySnow {
		cloudCol = sc.cloudDark
	}
	body := func(cx, cy float64) {
		if day {
			sun(p, cx, cy, u, sc.sun)
		} else {
			moon(p, cx, cy, u, sc.moon, r)
		}
	}
	switch kind {
	case skyClear:
		body(x+24*u, y+24*u)
	case skyPartly:
		body(x+18*u, y+17*u)
		cloud(p, x+6*u, y+16*u, u, cloudCol)
	case skyCloud:
		cloud(p, x+2*u, y+8*u, u*1.05, cloudCol)
	case skyFog:
		cloud(p, x+2*u, y+2*u, u, cloudCol)
		for i := 0; i < 3; i++ {
			yy := y + (34+float64(i)*5.5)*u
			r.fillRoundRect(p.img, x+(6+float64(i%2)*5)*u, yy, x+(42-float64(i%2)*4)*u, yy+2.6*u, 1.3*u, sc.cloud)
		}
	case skyRain, skyStorm, skySnow:
		cloud(p, x+2*u, y+2*u, u, cloudCol)
		for i := 0; i < 3; i++ {
			cx := x + (14+float64(i)*10)*u
			cy := y + 38*u
			switch kind {
			case skySnow:
				r.fillCircle(p.img, cx, cy, 2.4*u, sc.snow)
				r.fillCircle(p.img, cx-4*u, cy+6*u, 2*u, sc.snow)
			case skyStorm:
				if i == 1 {
					bolt(p, cx-4*u, cy-6*u, u, sc.bolt)
					continue
				}
				drop(p, cx, cy, u, sc.rain)
			default:
				drop(p, cx, cy, u, sc.rain)
			}
		}
	}
}

func sun(p avPaint, cx, cy, u float64, col color.RGBA) {
	r := p.r
	r.fillCircle(p.img, cx, cy, 8.5*u, col)
	for i := 0; i < 8; i++ {
		a := float64(i) * math.Pi / 4
		x0, y0 := cx+math.Cos(a)*12.5*u, cy+math.Sin(a)*12.5*u
		x1, y1 := cx+math.Cos(a)*17*u, cy+math.Sin(a)*17*u
		line(p, x0, y0, x1, y1, 2.6*u, col)
	}
}

func moon(p avPaint, cx, cy, u float64, col color.RGBA, r *renderer) {
	pb := r.path(p.img)
	pb.poly(crescent(cx, cy, 11*u, cx+6*u, cy-4.5*u, 9*u))
	pb.fill(col)
}

// crescent is the outline of the disc (ox, oy, or) with the disc (kx, ky,
// kr) taken out of it.
func crescent(ox, oy, or, kx, ky, kr float64) [][2]float64 {
	const n = 96
	in := func(x, y, cx, cy, rad float64) bool { return math.Hypot(x-cx, y-cy) < rad }
	// Round the outer edge, starting inside the bite, keeping what's outside it.
	start := math.Atan2(ky-oy, kx-ox)
	var edge [][2]float64
	for i := 0; i <= n; i++ {
		a := start + 2*math.Pi*float64(i)/n
		x, y := ox+or*math.Cos(a), oy+or*math.Sin(a)
		if !in(x, y, kx, ky, kr) {
			edge = append(edge, [2]float64{x, y})
		}
	}
	// Back along the bite's edge, starting outside the disc, keeping what's inside it.
	start = math.Atan2(oy-ky, ox-kx) + math.Pi
	var bite [][2]float64
	for i := 0; i <= n; i++ {
		a := start + 2*math.Pi*float64(i)/n
		x, y := kx+kr*math.Cos(a), ky+kr*math.Sin(a)
		if in(x, y, ox, oy, or) {
			bite = append(bite, [2]float64{x, y})
		}
	}
	if len(edge) == 0 || len(bite) == 0 {
		return edge
	}
	last := edge[len(edge)-1]
	if math.Hypot(bite[0][0]-last[0], bite[0][1]-last[1]) > math.Hypot(bite[len(bite)-1][0]-last[0], bite[len(bite)-1][1]-last[1]) {
		for i, j := 0, len(bite)-1; i < j; i, j = i+1, j-1 {
			bite[i], bite[j] = bite[j], bite[i]
		}
	}
	return append(edge, bite...)
}

func cloud(p avPaint, x, y, u float64, col color.RGBA) {
	r := p.r
	r.fillCircle(p.img, x+14*u, y+22*u, 8*u, col)
	r.fillCircle(p.img, x+24*u, y+16*u, 11*u, col)
	r.fillCircle(p.img, x+34*u, y+22*u, 8*u, col)
	r.fillRoundRect(p.img, x+6*u, y+20*u, x+42*u, y+30*u, 5*u, col)
}

func drop(p avPaint, cx, cy, u float64, col color.RGBA) {
	r := p.r
	pb := r.path(p.img)
	pts := [][2]float64{{cx, cy - 5*u}}
	pts = append(pts, arc(nil, cx, cy+1.5*u, 3*u, -math.Pi*0.15, math.Pi*1.15)...)
	pb.poly(pts)
	pb.fill(col)
}

func bolt(p avPaint, x, y, u float64, col color.RGBA) {
	pb := p.r.path(p.img)
	pb.poly([][2]float64{{x + 6*u, y}, {x + 1*u, y + 8*u}, {x + 4.5*u, y + 8*u}, {x + 2*u, y + 15*u}, {x + 9*u, y + 5.5*u}, {x + 5.5*u, y + 5.5*u}, {x + 8*u, y}})
	pb.fill(col)
}

// line draws a round-capped stroke.
func line(p avPaint, x0, y0, x1, y1, w float64, col color.RGBA) {
	r := p.r
	dx, dy := x1-x0, y1-y0
	l := math.Hypot(dx, dy)
	if l == 0 {
		return
	}
	nx, ny := -dy/l*w/2, dx/l*w/2
	pb := r.path(p.img)
	pb.poly([][2]float64{{x0 + nx, y0 + ny}, {x1 + nx, y1 + ny}, {x1 - nx, y1 - ny}, {x0 - nx, y0 - ny}})
	pb.fill(col)
	r.fillCircle(p.img, x0, y0, w/2, col)
	r.fillCircle(p.img, x1, y1, w/2, col)
}

// --- the card ------------------------------------------------------------------

func deg(t float64) string { return fmt.Sprintf("%.0f°", math.Round(t)) }

// drawWeather draws the weather card from y and returns where it ends.
func (v *activityView) drawWeather(p avPaint, c avColors, x0, x1, y float64) float64 {
	r := p.r
	ix0, ix1 := x0+avPad, x1-avPad
	w := v.weather
	if w == nil {
		if !v.weatherSet {
			return y // nothing asked for: no card
		}
		h := 56.0
		p.box(x0, y, x1, y+h, avRadius, c.card)
		p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
		msg := "Getting the weather…"
		if v.weatherErr != "" {
			msg = v.weatherErr
		}
		p.text(msg, ix0, y+33, 13.5, false, r.pal.gray, ix1-ix0)
		return y + h
	}
	narrow := ix1-ix0 < 300
	hoursH := 0.0
	if len(w.Hours) > 0 {
		hoursH = 74
	}
	h := avPad + 64 + 30 + hoursH + avPad
	if narrow {
		h += 18
	}
	p.box(x0, y, x1, y+h, avRadius, c.card)
	p.outline(x0, y, x1, y+h, avRadius, c.cardLine)
	if p.on() {
		v.hits = append(v.hits, avHit{x0, y, x1, y + h, "weather"})
	}

	// The place, the icon, the temperature and what it's like.
	top := y + avPad
	drawSky(p, w.Code, w.Day, ix0-2, top+4, 56)
	tx := ix0 + 66
	tw := p.text(deg(w.Temp), tx, top+42, 38, false, r.pal.text, 0)
	p.text(weatherText(w.Code), tx+tw+12, top+24, 14.5, true, r.pal.text, ix1-tx-tw-12)
	p.text(w.Place, tx+tw+12, top+43, 12, false, r.pal.gray, ix1-tx-tw-12)
	p.text(fmt.Sprintf("H %s  L %s  · feels %s", deg(w.High), deg(w.Low), deg(w.Feels)), tx, top+62, 12.5, false, r.pal.gray, ix1-tx)

	// Wind, humidity, rain.
	sy := top + 92
	wind := fmt.Sprintf("%.0f km/h", w.Wind)
	if w.F {
		wind = fmt.Sprintf("%.0f mph", w.Wind)
	}
	stats := []string{"Wind " + wind, fmt.Sprintf("Humidity %d%%", w.Humidity), fmt.Sprintf("Rain %d%%", w.Rain)}
	if narrow {
		p.text(stats[0]+" · "+stats[1], ix0, sy, 12, false, r.pal.gray, ix1-ix0)
		p.text(stats[2], ix0, sy+18, 12, false, r.pal.gray, ix1-ix0)
		sy += 18
	} else {
		p.text(strings.Join(stats, "   ·   "), ix0, sy, 12, false, r.pal.gray, ix1-ix0)
	}

	// The next hours.
	if len(w.Hours) > 0 {
		hy := sy + 14
		p.rect(ix0, hy, ix1, hy+1, c.cardLine)
		n := len(w.Hours)
		slot := (ix1 - ix0) / float64(n)
		for i, hr := range w.Hours {
			cx := ix0 + slot*(float64(i)+0.5)
			label := strings.ToLower(hr.At.Format("3pm"))
			lw := r.width(r.clean(label, false), 11.5, false)
			p.text(label, cx-lw/2, hy+20, 11.5, false, r.pal.faint, 0)
			drawSky(p, hr.Code, hr.Day, cx-12, hy+26, 24)
			t := deg(hr.Temp)
			tw := r.width(r.clean(t, true), 13, true)
			p.text(t, cx-tw/2, hy+68, 13, true, r.pal.text, 0)
		}
	}
	return y + h
}
