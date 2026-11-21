FROM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /recorder .

FROM alpine:3.20
RUN apk add --no-cache ffmpeg rclone tzdata ca-certificates
COPY --from=build /recorder /usr/local/bin/recorder
ENV DATA_DIR=/data RECORDER_ADDR=0.0.0.0:8081
VOLUME /data
CMD ["recorder"]
