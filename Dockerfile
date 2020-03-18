FROM debian:stretch
ENV COORD_PATH /coord
RUN apt update && apt install -y python3
ADD coord /usr/local/bin/coord
ADD spore /usr/local/bin/spore
EXPOSE 5292
