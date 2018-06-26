<?php

namespace App\Api\V1\Controller;

use App\Entity\{ApiToken, User};
use App\Repository\{ApiTokenRepository, UserRepository};
use Doctrine\ORM\EntityManagerInterface;
use Symfony\Component\HttpFoundation\{JsonResponse, Request};
use Symfony\Component\Security\Core\Encoder\UserPasswordEncoderInterface;

class SecurityController extends AbstractApiController
{
    public function login(
        Request $request,
        EntityManagerInterface $em,
        UserRepository $userRepo,
        ApiTokenRepository $tokenRepo,
        UserPasswordEncoderInterface $passwordEncoder
    ): JsonResponse {
        $username = $request->request->get('username');
        $password = $request->request->get('password');

        /** @var User $user */
        if (null === $user = $userRepo->findOneBy(['username' => $username])) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_UNAUTHORIZED, 'User not found');
        }

        if (!$passwordEncoder->isPasswordValid($user, $password)) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_UNAUTHORIZED, 'Invalid password');
        }

        $apiToken = new ApiToken($user);
        $tokenRepo->add($apiToken);

        try {
            $em->flush();
        } catch (\Exception $ex) {
            return $this->createJsonResponse(null, [], JsonResponse::HTTP_INTERNAL_SERVER_ERROR, 'Token persisting error');
        }

        return $this->createJsonResponse($apiToken->getKey());
    }
}