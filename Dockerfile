FROM debian:stretch
ENV COORD_PATH /coord
ADD coord /usr/local/bin/coord
ADD spore /usr/local/bin/spore
EXPOSE 5292
