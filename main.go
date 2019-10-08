package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis"
	"github.com/google/uuid"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"sort"
	"strings"
	"syscall"
)

var vc *discordgo.VoiceConnection
var rc *redis.Client
var playing bool

type Sound struct {
	Name string
	Type string
}

type Sounds []Sound

func (s Sounds) Len() int           { return len(s) }
func (s Sounds) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s Sounds) Less(i, j int) bool { return strings.ToLower(s[i].Name) < strings.ToLower(s[j].Name) }

func main() {
	var port string = ":9076"

	rc = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379", // use default Addr
		Password: "",               // no password set
		DB:       0,                // use default DB
	})

	guild := flag.String("server", "", "server id")
	channel := flag.String("channel", "", "channel id")
	token := flag.String("token", "", "token")
	httponly := flag.Bool("httponly", false, "only start server")

	flag.Parse()

	http.HandleFunc("/", indexPage)
	http.HandleFunc("/upload", upload)
	http.HandleFunc("/play", play)
	http.HandleFunc("/favorite", favorite)
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		w.WriteHeader(http.StatusOK)
	})

	log.Println("Listening on", port)

	if *httponly {
		http.ListenAndServe(port, nil)
		return
	}

	go http.ListenAndServe(port, nil)

	// setup discord session
	session, err := discordgo.New("Bot " + *token)
	if err != nil {
		log.Fatal(err)
	}

	session.Open()

	vc, err = session.ChannelVoiceJoin(*guild, *channel, false, true)
	if err != nil {
		log.Fatal(err)
	}
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
	vc.Disconnect()
	session.Close()
}

func play(w http.ResponseWriter, r *http.Request) {
	if playing {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	playing = true

	name := r.URL.Query().Get("name")

	sound, err := loadSound(name)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	vc.Speaking(true)

	for _, b := range sound {
		vc.OpusSend <- b
	}

	vc.Speaking(false)
	playing = false
}

func favorite(w http.ResponseWriter, r *http.Request) {
	// Get session ID cookie
	var sessionID string
	sessionCookie, err := r.Cookie("session")
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if sessionCookie == nil {
		sessionID = uuid.New().String()
	} else {
		sessionID = sessionCookie.Value
	}

	name := r.URL.Query().Get("name")
	if name == "" {
		log.Println(err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// See if this already exists in the session's favorites set
	exists, err := rc.SIsMember(fmt.Sprintf("favorites:%s", sessionID), name).Result()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if exists {
		_, err = rc.SRem(fmt.Sprintf("favorites:%s", sessionID), name).Result()
		if err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	_, err = rc.SAdd(fmt.Sprintf("favorites:%s", sessionID), name).Result()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func upload(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()

	if r.Method == "GET" {
		// Render upload form
		uploadTemplate := template.Must(template.ParseFiles("layouts/upload.html"))
		err := uploadTemplate.ExecuteTemplate(w, "upload", nil)
		if err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
		}

		return
	}

	if r.Method != "POST" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// process upload
	name := strings.Replace(r.FormValue("name"), " ", "-", -1)
	sound, header, err := r.FormFile("sound")
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	defer sound.Close()

	err = saveSound(sound, header.Filename, name)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", 302)

}

func indexPage(w http.ResponseWriter, r *http.Request) {
	// Set the session cookie if it's not present
	sessionCookie, err := r.Cookie("session")
	if err != nil {
		http.SetCookie(w, &http.Cookie{
			Name:  "session",
			Value: uuid.New().String(),
		})
	}

	_ = sessionCookie

	// Read the favorites from redis
	favorites, err := rc.SMembers(fmt.Sprintf("favorites:%s", sessionCookie.Value)).Result()
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Define IndexPage struct for use in template
	type IndexPage struct {
		Sounds    Sounds
		Favorites Sounds
	}

	defer r.Body.Close()

	// read sounds dir and list sounds
	files, err := ioutil.ReadDir("sounds")
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	sounds := Sounds{}

	for i := range files {
		sounds = append(sounds, Sound{Name: strings.TrimSuffix(files[i].Name(), ".dca")})
	}

	sort.Sort(sounds)

	page := IndexPage{
		Sounds: sounds,
	}

	// Form favorites
	for _, s := range sounds {
		for _, f := range favorites {
			if s.Name == f {
				page.Favorites = append(page.Favorites, s)
			}
		}
	}

	// Load Template
	indexTemplate := template.Must(template.ParseFiles("layouts/index.html"))

	err = indexTemplate.ExecuteTemplate(w, "index", page)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func loadSound(name string) ([][]byte, error) {

	var buffer [][]byte

	file, err := os.Open(path.Join("sounds", name+".dca"))
	if err != nil {
		return buffer, err
	}

	var opuslen int16

	for {
		// Read opus frame length from dca file.
		err = binary.Read(file, binary.LittleEndian, &opuslen)

		// If this is the end of the file, just return.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err := file.Close()
			if err != nil {
				return buffer, fmt.Errorf("Error reading frame length?: %v", err)
			}
			return buffer, nil
		}

		if err != nil {
			return buffer, err
		}

		// Read encoded pcm from dca file.
		InBuf := make([]byte, opuslen)
		err = binary.Read(file, binary.LittleEndian, &InBuf)

		// Should not be any end of file errors
		if err != nil {
			return buffer, fmt.Errorf("Error reading pcm: %v", err)
		}

		// Append encoded pcm data to the buffer.
		buffer = append(buffer, InBuf)
	}

	return buffer, nil
}

func saveSound(file multipart.File, filename, soundname string) error {
	// save file in uploaded
	// convert to dca and save in sounds
	p := path.Join("uploaded", filename)
	f, err := os.Create(p)
	if err != nil {
		return err
	}

	defer f.Close()

	_, err = io.Copy(f, file)
	if err != nil {
		return err
	}

	target := path.Join("sounds", soundname+".dca")
	cmd := fmt.Sprintf("ffmpeg -i %v -f s16le -ar 48000 -ac 2 pipe:1 | dca > %v", p, target)
	_, err = exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		return err
	}

	return nil
}
