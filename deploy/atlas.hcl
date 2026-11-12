// Atlas settings inside the migrate image (see Dockerfile): apply the migrations copied into the
// image to the database the app uses.
env "deploy" {
  url = getenv("DATABASE_URL")
  migration {
    dir = "file://migrations"
  }
}
